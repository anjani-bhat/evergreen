package model

import (
	"fmt"
	"sort"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/apimodels"
	"github.com/evergreen-ci/evergreen/db"
	"github.com/evergreen-ci/evergreen/model/build"
	"github.com/evergreen-ci/evergreen/model/commitqueue"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/patch"
	"github.com/evergreen-ci/evergreen/model/pod"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/model/testresult"
	"github.com/evergreen-ci/utility"
	adb "github.com/mongodb/anser/db"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
)

type StatusChanges struct {
	PatchNewStatus   string
	VersionNewStatus string
	VersionComplete  bool
	BuildNewStatus   string
	BuildComplete    bool
}

func SetActiveState(caller string, active bool, tasks ...task.Task) error {
	tasksToActivate := []task.Task{}
	versionIdsSet := map[string]bool{}
	buildToTaskMap := map[string]task.Task{}
	catcher := grip.NewBasicCatcher()
	for _, t := range tasks {
		originalTasks := []task.Task{t}
		if t.DisplayOnly {
			execTasks, err := task.Find(task.ByIds(t.ExecutionTasks))
			catcher.Wrap(err, "getting execution tasks")
			originalTasks = append(originalTasks, execTasks...)
		}
		versionIdsSet[t.Version] = true
		buildToTaskMap[t.BuildId] = t
		if active {
			// if the task is being activated, and it doesn't override its dependencies
			// activate the task's dependencies as well
			if !t.OverrideDependencies {
				deps, err := task.GetRecursiveDependenciesUp(originalTasks, nil)
				catcher.Wrapf(err, "getting dependencies up for task '%s'", t.Id)
				if t.IsPartOfSingleHostTaskGroup() {
					for _, dep := range deps {
						// reset any already finished tasks in the same task group
						if dep.TaskGroup == t.TaskGroup && t.TaskGroup != "" && dep.IsFinished() {
							catcher.Wrapf(resetTask(dep.Id, caller), "resetting dependency '%s'", dep.Id)
						} else {
							tasksToActivate = append(tasksToActivate, dep)
						}
					}
				} else {
					tasksToActivate = append(tasksToActivate, deps...)
				}
			}

			// Investigating strange dispatch state as part of EVG-13144
			if t.IsHostTask() && !utility.IsZeroTime(t.DispatchTime) && t.Status == evergreen.TaskUndispatched {
				catcher.Wrapf(resetTask(t.Id, caller), "resetting task '%s'", t.Id)
			} else {
				tasksToActivate = append(tasksToActivate, originalTasks...)
			}

			// If the task was not activated by step back, and either the caller is not evergreen
			// or the task was originally activated by evergreen, deactivate the task
		} else if !evergreen.IsSystemActivator(caller) || evergreen.IsSystemActivator(t.ActivatedBy) {
			// deactivate later tasks in the group as well, since they won't succeed without this one
			if t.IsPartOfSingleHostTaskGroup() {
				tasksInGroup, err := task.FindTaskGroupFromBuild(t.BuildId, t.TaskGroup)
				catcher.Wrapf(err, "finding task group '%s'", t.TaskGroup)
				for _, taskInGroup := range tasksInGroup {
					if taskInGroup.TaskGroupOrder > t.TaskGroupOrder {
						originalTasks = append(originalTasks, taskInGroup)
					}
				}
			}
			if t.Requester == evergreen.MergeTestRequester {
				catcher.Wrapf(DequeueAndRestartForTask(nil, &t, message.GithubStateError, caller, fmt.Sprintf("deactivated by '%s'", caller)), "dequeueing and restarting task '%s'", t.Id)
			}
			tasksToActivate = append(tasksToActivate, originalTasks...)
		} else {
			continue
		}
		if t.IsPartOfDisplay() {
			catcher.Wrap(UpdateDisplayTaskForTask(&t), "updating display task")
		}
	}

	if active {
		if err := task.ActivateTasks(tasksToActivate, time.Now(), true, caller); err != nil {
			return errors.Wrap(err, "activating tasks")
		}
		versionIdsToActivate := []string{}
		for v := range versionIdsSet {
			versionIdsToActivate = append(versionIdsToActivate, v)
		}
		if err := ActivateVersions(versionIdsToActivate); err != nil {
			return errors.Wrap(err, "marking version as activated")
		}
	} else {
		if err := task.DeactivateTasks(tasksToActivate, true, caller); err != nil {
			return errors.Wrap(err, "deactivating task")
		}
	}

	for b, item := range buildToTaskMap {
		t := buildToTaskMap[b]
		if err := UpdateBuildAndVersionStatusForTask(&item); err != nil {
			return errors.Wrapf(err, "updating build and version status for task '%s'", t.Id)
		}
	}

	return catcher.Resolve()
}

func SetActiveStateById(id, user string, active bool) error {
	t, err := task.FindOneId(id)
	if err != nil {
		return errors.Wrapf(err, "finding task '%s'", id)
	}
	if t == nil {
		return errors.Errorf("task '%s' not found", id)
	}
	return SetActiveState(user, active, *t)
}

// activatePreviousTask will set the Active state for the first task with a
// revision order number less than the current task's revision order number.
// originalStepbackTask is only specified if we're first activating the generator for a generated task.
// StepbackDepth should be reconsidered in EVG-17949 and is currently only used for logging.
// Depth passed in is the depth we should assign to the previous task.
func activatePreviousTask(taskId, caller string, originalStepbackTask *task.Task, stepbackDepth int) error {
	// find the task first
	t, err := task.FindOneId(taskId)
	if err != nil {
		return errors.WithStack(err)
	}
	if t == nil {
		return errors.Errorf("task '%s' does not exist", taskId)
	}

	// find previous task limiting to just the last one
	filter, sort := task.ByBeforeRevision(t.RevisionOrderNumber, t.BuildVariant, t.DisplayName, t.Project, t.Requester)
	query := db.Query(filter).Sort(sort)
	prevTask, err := task.FindOne(query)
	if err != nil {
		return errors.Wrap(err, "finding previous task")
	}

	// for generated tasks, try to activate the generator instead if the previous task we found isn't the actual last task
	if t.GeneratedBy != "" && prevTask != nil && prevTask.RevisionOrderNumber+1 != t.RevisionOrderNumber {
		return activatePreviousTask(t.GeneratedBy, caller, t, stepbackDepth)
	}

	// if this is the first time we're running the task, or it's finished, has a negative priority, or already activated
	if prevTask == nil || prevTask.IsFinished() || prevTask.Priority < 0 || prevTask.Activated {
		return nil
	}

	grip.Debug(message.Fields{
		"ticket":         "EVG-17949",
		"message":        "stepping back task",
		"stepback_depth": stepbackDepth,
		"project_id":     t.Project,
		"task_id":        t.Id,
	})

	// activate the task
	if err = SetActiveState(caller, true, *prevTask); err != nil {
		return errors.Wrapf(err, "setting task '%s' active", prevTask.Id)
	}
	// add the task that we're actually stepping back so that we know to activate it
	if prevTask.GenerateTask && originalStepbackTask != nil {
		if err = prevTask.SetGeneratedTasksToActivate(originalStepbackTask.BuildVariant, originalStepbackTask.DisplayName); err != nil {
			return errors.Wrap(err, "setting generated tasks to activate")
		}
	}
	if stepbackDepth > 0 {
		if err = prevTask.SetStepbackDepth(stepbackDepth); err != nil {
			return errors.Wrap(err, "setting stepback depth")
		}
	}
	return nil
}

func resetManyTasks(tasks []task.Task, caller string) error {
	catcher := grip.NewBasicCatcher()
	for _, t := range tasks {
		catcher.Add(resetTask(t.Id, caller))
	}
	return catcher.Resolve()
}

// TryResetTask resets a task. Individual execution tasks cannot be reset - to
// reset an execution task, the given task ID must be that of its parent display
// task.
func TryResetTask(taskId, user, origin string, detail *apimodels.TaskEndDetail) error {
	t, err := task.FindOneId(taskId)
	if err != nil {
		return errors.WithStack(err)
	}
	if t == nil {
		return errors.Errorf("cannot restart task '%s' because it could not be found", taskId)
	}
	if t.IsPartOfDisplay() {
		return errors.Errorf("cannot restart execution task '%s' because it is part of a display task", t.Id)
	}

	var execTask *task.Task

	// if we've reached the max number of executions for this task, mark it as finished and failed
	if t.Execution >= evergreen.MaxTaskExecution {
		// restarting from the UI bypasses the restart cap
		msg := fmt.Sprintf("task '%s' reached max execution %d: ", t.Id, evergreen.MaxTaskExecution)
		if origin == evergreen.UIPackage || origin == evergreen.RESTV2Package {
			grip.Debugln(msg, "allowing exception for", user)
		} else if !t.IsFinished() {
			if detail != nil {
				grip.Debugln(msg, "marking as failed")
				if t.DisplayOnly {
					for _, etId := range t.ExecutionTasks {
						execTask, err = task.FindOneId(etId)
						if err != nil {
							return errors.Wrap(err, "finding execution task")
						}
						if err = MarkEnd(execTask, origin, time.Now(), detail, false); err != nil {
							return errors.Wrap(err, "marking execution task as ended")
						}
					}
				}
				return errors.WithStack(MarkEnd(t, origin, time.Now(), detail, false))
			} else {
				grip.Critical(message.Fields{
					"message":     "TryResetTask called with nil TaskEndDetail",
					"origin":      origin,
					"task_id":     taskId,
					"task_status": t.Status,
				})
			}
		} else {
			return nil
		}
	}

	// only allow re-execution for failed or successful tasks
	if !t.IsFinished() {
		// this is to disallow terminating running tasks via the UI
		if origin == evergreen.UIPackage || origin == evergreen.RESTV2Package {
			grip.Debugf("Unsatisfiable '%s' reset request on '%s' (status: '%s')",
				user, t.Id, t.Status)
			if t.DisplayOnly {
				execTasks := map[string]string{}
				for _, et := range t.ExecutionTasks {
					execTask, err = task.FindOneId(et)
					if err != nil {
						continue
					}
					execTasks[execTask.Id] = execTask.Status
				}
				grip.Error(message.Fields{
					"message":    "attempt to restart unfinished display task",
					"task":       t.Id,
					"status":     t.Status,
					"exec_tasks": execTasks,
				})
			}
			return errors.Errorf("task '%s' currently has status '%s' - cannot reset task in this status",
				t.Id, t.Status)
		}
	}

	if detail != nil {
		if err = t.MarkEnd(time.Now(), detail); err != nil {
			return errors.Wrap(err, "marking task as ended")
		}
	}

	caller := origin
	if origin == evergreen.UIPackage || origin == evergreen.RESTV2Package {
		caller = user
	}
	if t.IsPartOfSingleHostTaskGroup() {
		if err = t.SetResetWhenFinished(); err != nil {
			return errors.Wrap(err, "marking task group for reset")
		}
		return errors.Wrap(checkResetSingleHostTaskGroup(t, caller), "resetting single host task group")
	}

	return errors.WithStack(resetTask(t.Id, caller))
}

// resetTask finds a finished task, attempts to archive it, and resets the task and
// resets the TaskCache in the build as well.
func resetTask(taskId, caller string) error {
	t, err := task.FindOneId(taskId)
	if err != nil {
		return errors.WithStack(err)
	}
	if t.IsPartOfDisplay() {
		return errors.Errorf("cannot restart execution task '%s' because it is part of a display task", t.Id)
	}
	if err = t.Archive(); err != nil {
		return errors.Wrap(err, "can't restart task because it can't be archived")
	}

	if err = MarkOneTaskReset(t); err != nil {
		return errors.WithStack(err)
	}
	event.LogTaskRestarted(t.Id, t.Execution, caller)

	if err := t.ActivateTask(caller); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(UpdateBuildAndVersionStatusForTask(t))
}

func AbortTask(taskId, caller string) error {
	t, err := task.FindOneId(taskId)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.Errorf("task '%s' not found", taskId)
	}
	if t.DisplayOnly {
		for _, et := range t.ExecutionTasks {
			_ = AbortTask(et, caller) // discard errors because some execution tasks may not be abortable
		}
	}

	if !t.IsAbortable() {
		return errors.Errorf("task '%s' currently has status '%s' - cannot abort task"+
			" in this status", t.Id, t.Status)
	}

	// set the active state and then set the abort
	if err = SetActiveState(caller, false, *t); err != nil {
		return err
	}
	event.LogTaskAbortRequest(t.Id, t.Execution, caller)
	return t.SetAborted(task.AbortInfo{User: caller})
}

// DeactivatePreviousTasks deactivates any previously activated but undispatched
// tasks for the same build variant + display name + project combination
// as the task.
func DeactivatePreviousTasks(t *task.Task, caller string) error {
	filter, sort := task.ByActivatedBeforeRevisionWithStatuses(
		t.RevisionOrderNumber,
		[]string{evergreen.TaskUndispatched},
		t.BuildVariant,
		t.DisplayName,
		t.Project,
	)
	query := db.Query(filter).Sort(sort)
	allTasks, err := task.FindAll(query)
	if err != nil {
		return errors.Wrapf(err, "finding previous tasks to deactivate for task '%s'", t.Id)
	}
	extraTasks := []task.Task{}
	if t.DisplayOnly {
		for _, dt := range allTasks {
			if len(dt.ExecutionTasks) == 0 { // previous display tasks may not have execution tasks added yet
				continue
			}
			var execTasks []task.Task
			execTasks, err = task.Find(task.ByIds(dt.ExecutionTasks))
			if err != nil {
				return errors.Wrapf(err, "finding execution tasks to deactivate for task '%s'", dt.Id)
			}
			canDeactivate := true
			for _, et := range execTasks {
				if et.IsFinished() || et.IsAbortable() {
					canDeactivate = false
					break
				}
			}
			if canDeactivate {
				extraTasks = append(extraTasks, execTasks...)
			}
		}
	}
	allTasks = append(allTasks, extraTasks...)

	for _, t := range allTasks {
		if err = SetActiveState(caller, false, t); err != nil {
			return err
		}
	}

	return nil
}

// Returns true if the task should stepback upon failure, and false
// otherwise. Note that the setting is obtained from the top-level
// project, if not explicitly set on the task or disabled at the project level.
func getStepback(taskId string) (bool, error) {
	t, err := task.FindOneId(taskId)
	if err != nil {
		return false, errors.Wrapf(err, "finding task '%s'", taskId)
	}
	if t == nil {
		return false, errors.Errorf("task '%s' not found", taskId)
	}
	projectRef, err := FindMergedProjectRef(t.Project, "", false)
	if err != nil {
		return false, errors.Wrapf(err, "finding merged project ref for task '%s'", taskId)
	}
	if projectRef == nil {
		return false, errors.Errorf("project for task '%s' not found", taskId)
	}
	// Disabling the feature at the project level takes precedent.
	if projectRef.IsStepbackDisabled() {
		return false, nil
	}

	project, err := FindProjectFromVersionID(t.Version)
	if err != nil {
		return false, errors.WithStack(err)
	}

	projectTask := project.FindProjectTask(t.DisplayName)
	// Check if the task overrides the stepback policy specified by the project
	if projectTask != nil && projectTask.Stepback != nil {
		return *projectTask.Stepback, nil
	}

	// Check if the build variant overrides the stepback policy specified by the project
	for _, buildVariant := range project.BuildVariants {
		if t.BuildVariant == buildVariant.Name {
			if buildVariant.Stepback != nil {
				return *buildVariant.Stepback, nil
			}
			break
		}
	}
	return project.Stepback, nil
}

// doStepBack performs a stepback on the task if there is a previous task and if not it returns nothing.
func doStepback(t *task.Task) error {
	if t.DisplayOnly {
		execTasks, err := task.Find(task.ByIds(t.ExecutionTasks))
		if err != nil {
			return errors.Wrapf(err, "finding tasks for stepback of '%s'", t.Id)
		}
		catcher := grip.NewSimpleCatcher()
		for _, et := range execTasks {
			catcher.Add(doStepback(&et))
		}
		if catcher.HasErrors() {
			return catcher.Resolve()
		}
	}

	//See if there is a prior success for this particular task.
	//If there isn't, we should not activate the previous task because
	//it could trigger stepping backwards ad infinitum.
	prevTask, err := t.PreviousCompletedTask(t.Project, []string{evergreen.TaskSucceeded})
	if err != nil {
		return errors.Wrap(err, "locating previous successful task")
	}
	if prevTask == nil {
		return nil
	}

	// activate the previous task to pinpoint regression
	return errors.WithStack(activatePreviousTask(t.Id, evergreen.StepbackTaskActivator, nil, t.StepbackDepth+1))
}

// MarkEnd updates the task as being finished, performs a stepback if necessary, and updates the build status
func MarkEnd(t *task.Task, caller string, finishTime time.Time, detail *apimodels.TaskEndDetail,
	deactivatePrevious bool) error {
	const slowThreshold = time.Second

	detailsCopy := *detail
	hasFailedTests, err := t.HasFailedTests()
	if err != nil {
		return errors.Wrap(err, "checking for failed tests")
	}
	if hasFailedTests && detailsCopy.Status != evergreen.TaskFailed {
		detailsCopy.Type = evergreen.CommandTypeTest
		detailsCopy.Status = evergreen.TaskFailed
	}

	if t.Status == detailsCopy.Status {
		grip.Warning(message.Fields{
			"message": "tried to mark task as finished twice",
			"task":    t.Id,
		})
		return nil
	}
	if !t.HasCedarResults { // Results not in cedar, check the db.
		count, err := testresult.Count(testresult.FilterByTaskIDAndExecution(t.Id, t.Execution))
		if err != nil {
			return errors.Wrap(err, "unable to count test results")
		}
		t.HasLegacyResults = utility.ToBoolPtr(count > 0) // cache if we even need to look this up in the future

		if detailsCopy.Status == evergreen.TaskSucceeded && count == 0 && t.MustHaveResults {
			detailsCopy.Status = evergreen.TaskFailed
			detailsCopy.Description = evergreen.TaskDescriptionNoResults
		}
	}

	t.Details = detailsCopy
	if utility.IsZeroTime(t.StartTime) {
		grip.Warning(message.Fields{
			"message":      "task is missing start time",
			"task_id":      t.Id,
			"execution":    t.Execution,
			"requester":    t.Requester,
			"activated_by": t.ActivatedBy,
		})
	}
	startPhaseAt := time.Now()
	err = t.MarkEnd(finishTime, &detailsCopy)
	grip.NoticeWhen(time.Since(startPhaseAt) > slowThreshold, message.Fields{
		"message":       "slow operation",
		"function":      "MarkEnd",
		"step":          "t.MarkEnd",
		"task":          t.Id,
		"duration_secs": time.Since(startPhaseAt).Seconds(),
	})

	if err != nil {
		return errors.Wrap(err, "marking task finished")
	}

	if err = UpdateBlockedDependencies(t); err != nil {
		return errors.Wrap(err, "updating blocked dependencies")
	}

	if err = t.MarkDependenciesFinished(true); err != nil {
		return errors.Wrap(err, "updating dependency met status")
	}

	status := t.GetDisplayStatus()
	event.LogTaskFinished(t.Id, t.Execution, t.HostId, status)
	grip.Info(message.Fields{
		"message":            "marking task finished",
		"usage":              "container task health dashboard",
		"task_id":            t.Id,
		"execution":          t.Execution,
		"status":             status,
		"operation":          "MarkEnd",
		"host_id":            t.HostId,
		"execution_platform": t.ExecutionPlatform,
	})

	if t.IsPartOfDisplay() {
		if err = UpdateDisplayTaskForTask(t); err != nil {
			return errors.Wrap(err, "updating display task")
		}
		dt, err := t.GetDisplayTask()
		if err != nil {
			return errors.Wrap(err, "getting display task")
		}
		if err = checkResetDisplayTask(dt); err != nil {
			return errors.Wrap(err, "checking display task reset")
		}
	} else {
		if t.IsPartOfSingleHostTaskGroup() {
			if err = checkResetSingleHostTaskGroup(t, caller); err != nil {
				return errors.Wrap(err, "resetting task group")
			}
		}
	}

	// activate/deactivate other task if this is not a patch request's task
	if !evergreen.IsPatchRequester(t.Requester) {
		if t.IsPartOfDisplay() {
			_, err = t.GetDisplayTask()
			if err != nil {
				return errors.Wrap(err, "getting display task")
			}
			err = evalStepback(t.DisplayTask, caller, t.DisplayTask.Status, deactivatePrevious)
		} else {
			err = evalStepback(t, caller, status, deactivatePrevious)
		}
		if err != nil {
			return err
		}
	}

	if err := UpdateBuildAndVersionStatusForTask(t); err != nil {
		return errors.Wrap(err, "updating build/version status")
	}

	if (t.ResetWhenFinished || t.ResetFailedWhenFinished) && !t.IsPartOfDisplay() && !t.IsPartOfSingleHostTaskGroup() {
		return TryResetTask(t.Id, evergreen.APIServerTaskActivator, "", detail)
	}

	return nil
}

// UpdateBlockedDependencies traverses the dependency graph and recursively sets each
// parent dependency as unattainable in depending tasks.
func UpdateBlockedDependencies(t *task.Task) error {
	dependentTasks, err := t.FindAllUnmarkedBlockedDependencies()
	if err != nil {
		return errors.Wrapf(err, "getting tasks depending on task '%s'", t.Id)
	}

	for _, dependentTask := range dependentTasks {
		if err = dependentTask.MarkUnattainableDependency(t.Id, true); err != nil {
			return errors.Wrap(err, "marking dependency unattainable")
		}
		if err = UpdateBlockedDependencies(&dependentTask); err != nil {
			return errors.Wrapf(err, "updating blocked dependencies for '%s'", t.Id)
		}
	}
	return nil
}

// UpdateUnblockedDependencies recursively marks all unattainable dependencies as attainable.
func UpdateUnblockedDependencies(t *task.Task) error {
	blockedTasks, err := t.FindAllMarkedUnattainableDependencies()
	if err != nil {
		return errors.Wrap(err, "getting dependencies marked unattainable")
	}

	for _, blockedTask := range blockedTasks {
		if err = blockedTask.MarkUnattainableDependency(t.Id, false); err != nil {
			return errors.Wrap(err, "marking dependency attainable")
		}

		if err := UpdateUnblockedDependencies(&blockedTask); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func RestartItemsAfterVersion(cq *commitqueue.CommitQueue, project, version, caller string) error {
	if cq == nil {
		var err error
		cq, err = commitqueue.FindOneId(project)
		if err != nil {
			return errors.Wrapf(err, "getting commit queue for project '%s'", project)
		}
		if cq == nil {
			return errors.Errorf("commit queue for project '%s' not found", project)
		}
	}

	foundItem := false
	catcher := grip.NewBasicCatcher()
	for _, item := range cq.Queue {
		if item.Version == "" {
			return nil
		}
		if item.Version == version {
			foundItem = true
		} else if foundItem && item.Version != "" {
			grip.Info(message.Fields{
				"message":            "restarting items due to commit queue failure",
				"failing_version":    version,
				"restarting_version": item.Version,
				"project":            project,
				"caller":             caller,
			})
			// this block executes on all items after the given task
			catcher.Add(RestartTasksInVersion(item.Version, true, caller))
		}
	}

	return catcher.Resolve()
}

// DequeueAndRestartForTask restarts all items after the given task's version, aborts/dequeues the current version,
// and sends an updated status to GitHub.
func DequeueAndRestartForTask(cq *commitqueue.CommitQueue, t *task.Task, githubState message.GithubState, caller, reason string) error {
	if cq == nil {
		var err error
		cq, err = commitqueue.FindOneId(t.Project)
		if err != nil {
			return errors.Wrapf(err, "getting commit queue for project '%s'", t.Project)
		}
		if cq == nil {
			return errors.Errorf("commit queue for project '%s' not found", t.Project)
		}
	}
	// this must be done before dequeuing so that we know which entries to restart
	if err := RestartItemsAfterVersion(cq, t.Project, t.Version, caller); err != nil {
		return errors.Wrapf(err, "restarting items after version '%s'", t.Version)
	}

	p, err := patch.FindOneId(t.Version)
	if err != nil {
		return errors.Wrap(err, "finding patch")
	}
	if p == nil {
		return errors.Errorf("patch '%s' not found", t.Version)
	}
	if err := tryDequeueAndAbortCommitQueueVersion(p, *cq, t.Id, caller); err != nil {
		return err
	}

	err = SendCommitQueueResult(p, githubState, reason)
	grip.Error(message.WrapError(err, message.Fields{
		"message": "unable to send github status",
		"patch":   t.Version,
	}))

	return nil
}

// HandleEndTaskForCommitQueueTask handles necessary dequeues and stepback restarts for
// ending tasks that run on a commit queue.
func HandleEndTaskForCommitQueueTask(t *task.Task, status string) error {
	cq, err := commitqueue.FindOneId(t.Project)
	if err != nil {
		return errors.Wrapf(err, "can't get commit queue for id '%s'", t.Project)
	}
	if cq == nil {
		return errors.Errorf("no commit queue found for '%s'", t.Project)
	}
	if status != evergreen.TaskSucceeded && !t.Aborted {
		return dequeueAndRestartWithStepback(cq, t, evergreen.APIServerTaskActivator, fmt.Sprintf("task '%s' failed", t.DisplayName))
	} else if status == evergreen.TaskSucceeded {
		// Query for all cq version tasks after this one; they may have been waiting to see if this
		// one was the cause of the failure, in which case we should dequeue and restart.
		foundVersion := false
		for _, item := range cq.Queue {
			if item.Version == "" {
				return nil // no longer looking at scheduled versions
			}
			if item.Version == t.Version {
				foundVersion = true
				continue
			}
			if foundVersion {
				laterTask, err := task.FindTaskForVersion(item.Version, t.DisplayName, t.BuildVariant)
				if err != nil {
					return errors.Wrapf(err, "error finding task for version '%s'", item.Version)
				}
				if laterTask == nil {
					return errors.Errorf("couldn't find task for version '%s'", item.Version)
				}
				if evergreen.IsFailedTaskStatus(laterTask.Status) {
					// Because our task is successful, this task should have failed so we dequeue.
					return dequeueAndRestartWithStepback(cq, laterTask, evergreen.APIServerTaskActivator,
						fmt.Sprintf("task '%s' failed and was not impacted by previous task", t.DisplayName))
				}
				if !evergreen.IsFinishedTaskStatus(laterTask.Status) {
					// When this task finishes, it will handle stepping back any later commit queue item, so we're done.
					return nil
				}
			}

		}
	}
	return nil
}

// dequeueAndRestartWithStepback dequeues the current task and restarts later tasks, if earlier tasks have all run.
// Otherwise, the failure may be a result of those untested commits so we will wait for the earlier tasks to run
// and handle dequeuing (merge still won't run for failed task versions because of dependencies).
func dequeueAndRestartWithStepback(cq *commitqueue.CommitQueue, t *task.Task, caller, reason string) error {
	if i := cq.FindItem(t.Version); i > 0 {
		prevVersions := []string{}
		for j := 0; j < i; j++ {
			prevVersions = append(prevVersions, cq.Queue[j].Version)
		}
		// if any of the commit queue tasks higher on the queue haven't finished, then they will handle dequeuing.
		previousTaskNeedsToRun, err := task.HasUnfinishedTaskForVersions(prevVersions, t.DisplayName, t.BuildVariant)
		if err != nil {
			return errors.Wrap(err, "error checking early commit queue tasks")
		}
		if previousTaskNeedsToRun {
			return nil
		}
		// Otherwise, continue on and dequeue.
	}
	return DequeueAndRestartForTask(cq, t, message.GithubStateFailure, caller, reason)
}

func tryDequeueAndAbortCommitQueueVersion(p *patch.Patch, cq commitqueue.CommitQueue, taskId string, caller string) error {
	issue := p.Id.Hex()
	err := removeNextMergeTaskDependency(cq, issue)
	grip.Error(message.WrapError(err, message.Fields{
		"message": "error removing dependency",
		"patch":   issue,
	}))

	removed, err := cq.RemoveItemAndPreventMerge(issue, true, caller)
	grip.Debug(message.Fields{
		"message": "removing commit queue item",
		"issue":   issue,
		"err":     err,
		"removed": removed,
		"caller":  caller,
	})
	if err != nil {
		return errors.Wrapf(err, "removing and preventing merge for item '%s' from queue '%s'", p.Version, p.Project)
	}
	if removed == nil {
		return errors.Errorf("no commit queue entry removed for '%s'", issue)
	}

	if p.IsPRMergePatch() {
		err = SendCommitQueueResult(p, message.GithubStateFailure, "merge test failed")
		grip.Error(message.WrapError(err, message.Fields{
			"message": "error sending github status",
			"patch":   p.Id.Hex(),
		}))
	}

	event.LogCommitQueueConcludeTest(p.Id.Hex(), evergreen.MergeTestFailed)
	return errors.Wrap(CancelPatch(p, task.AbortInfo{TaskID: taskId, User: caller}), "aborting failed commit queue patch")
}

// removeNextMergeTaskDependency basically removes the given merge task from a linked list of
// merge task dependencies. It makes the next merge not depend on the current one and also makes
// the next merge depend on the previous one, if there is one
func removeNextMergeTaskDependency(cq commitqueue.CommitQueue, currentIssue string) error {
	currentIndex := cq.FindItem(currentIssue)
	if currentIndex < 0 {
		return errors.New("commit queue item not found")
	}
	if currentIndex+1 >= len(cq.Queue) {
		return nil
	}

	nextItem := cq.Queue[currentIndex+1]
	if nextItem.Version == "" {
		return nil
	}
	nextMerge, err := task.FindMergeTaskForVersion(nextItem.Version)
	if err != nil {
		return errors.Wrap(err, "finding next merge task")
	}
	if nextMerge == nil {
		return errors.New("no merge task found")
	}
	currentMerge, err := task.FindMergeTaskForVersion(cq.Queue[currentIndex].Version)
	if err != nil {
		return errors.Wrap(err, "finding current merge task")
	}
	if err = nextMerge.RemoveDependency(currentMerge.Id); err != nil {
		return errors.Wrap(err, "removing dependency")
	}

	if currentIndex > 0 {
		prevItem := cq.Queue[currentIndex-1]
		prevMerge, err := task.FindMergeTaskForVersion(prevItem.Version)
		if err != nil {
			return errors.Wrap(err, "finding previous merge task")
		}
		if prevMerge == nil {
			return errors.New("no merge task found")
		}
		d := task.Dependency{
			TaskId: prevMerge.Id,
			Status: AllStatuses,
		}
		if err = nextMerge.AddDependency(d); err != nil {
			return errors.Wrap(err, "adding dependency")
		}
	}

	return nil
}

func evalStepback(t *task.Task, caller, status string, deactivatePrevious bool) error {
	if status == evergreen.TaskFailed && !t.Aborted {
		var shouldStepBack bool
		shouldStepBack, err := getStepback(t.Id)
		if err != nil {
			return errors.WithStack(err)
		}
		if !shouldStepBack {
			return nil
		}

		if t.IsPartOfSingleHostTaskGroup() {
			// Stepback earlier task group tasks as well because these need to be run sequentially.
			catcher := grip.NewBasicCatcher()
			tasks, err := task.FindTaskGroupFromBuild(t.BuildId, t.TaskGroup)
			if err != nil {
				return errors.Wrapf(err, "getting task group for task '%s'", t.Id)
			}
			if len(tasks) == 0 {
				return errors.Errorf("no tasks in task group '%s' for task '%s'", t.TaskGroup, t.Id)
			}
			for _, tgTask := range tasks {
				catcher.Wrapf(doStepback(&tgTask), "stepping back task group task '%s'", tgTask.DisplayName)
				if tgTask.Id == t.Id {
					break // don't need to stepback later tasks in the group
				}
			}

			return catcher.Resolve()
		}
		return errors.Wrap(doStepback(t), "performing stepback")

	} else if status == evergreen.TaskSucceeded && deactivatePrevious && t.Requester == evergreen.RepotrackerVersionRequester {
		// if the task was successful and is a mainline commit (not git tag or project trigger),
		// ignore running previous activated tasks for this buildvariant
		if err := DeactivatePreviousTasks(t, caller); err != nil {
			return errors.Wrap(err, "deactivating previous task")
		}
	}

	return nil
}

// updateMakespans
func updateMakespans(b *build.Build, buildTasks []task.Task) error {
	depPath := FindPredictedMakespan(buildTasks)
	return errors.WithStack(b.UpdateMakespans(depPath.TotalTime, CalculateActualMakespan(buildTasks)))
}

// getBuildStatus returns a string denoting the status of the build and
// a boolean denoting if all tasks in the build are blocked.
func getBuildStatus(buildTasks []task.Task) (string, bool) {
	// Check if no tasks have started and if all tasks are blocked.
	noStartedTasks := true
	allTasksBlocked := true
	for _, t := range buildTasks {
		if !evergreen.IsUnstartedTaskStatus(t.Status) {
			noStartedTasks = false
			allTasksBlocked = false
			break
		}
		if !t.Blocked() {
			allTasksBlocked = false
		}
	}

	if noStartedTasks || allTasksBlocked {
		return evergreen.BuildCreated, allTasksBlocked
	}

	// Check if tasks are started but not finished.
	for _, t := range buildTasks {
		if t.Status == evergreen.TaskStarted {
			return evergreen.BuildStarted, false
		}
		if t.Activated && !t.Blocked() && !t.IsFinished() {
			return evergreen.BuildStarted, false
		}
	}

	// Check if all tasks are finished but have failures.
	for _, t := range buildTasks {
		if evergreen.IsFailedTaskStatus(t.Status) || t.Aborted {
			return evergreen.BuildFailed, false
		}
	}

	return evergreen.BuildSucceeded, false
}

func updateBuildGithubStatus(b *build.Build, buildTasks []task.Task) error {
	githubStatusTasks := make([]task.Task, 0, len(buildTasks))
	for _, t := range buildTasks {
		if t.IsGithubCheck {
			githubStatusTasks = append(githubStatusTasks, t)
		}
	}
	if len(githubStatusTasks) == 0 {
		return nil
	}

	githubBuildStatus, _ := getBuildStatus(githubStatusTasks)

	if githubBuildStatus == b.GithubCheckStatus {
		return nil
	}

	if evergreen.IsFinishedBuildStatus(githubBuildStatus) {
		event.LogBuildGithubCheckFinishedEvent(b.Id, githubBuildStatus)
	}

	return b.UpdateGithubCheckStatus(githubBuildStatus)
}

// updateBuildStatus updates the status of the build based on its tasks' statuses
// Returns true if the build's status has changed or if all of the build's tasks become blocked.
func updateBuildStatus(b *build.Build) (bool, error) {
	buildTasks, err := task.FindWithFields(task.ByBuildId(b.Id), task.StatusKey, task.ActivatedKey, task.DependsOnKey, task.IsGithubCheckKey, task.AbortedKey)
	if err != nil {
		return false, errors.Wrapf(err, "getting tasks in build '%s'", b.Id)
	}

	buildStatus, allTasksBlocked := getBuildStatus(buildTasks)
	blockedChanged := allTasksBlocked != b.AllTasksBlocked

	if err = b.SetAllTasksBlocked(allTasksBlocked); err != nil {
		return false, errors.Wrapf(err, "setting build '%s' as blocked", b.Id)
	}

	if buildStatus == b.Status {
		return blockedChanged, nil
	}

	// Only check aborted if status has changed.
	isAborted := false
	var taskStatuses []string
	for _, t := range buildTasks {
		if t.Aborted {
			isAborted = true
		} else {
			taskStatuses = append(taskStatuses, t.Status)
		}
	}
	isAborted = len(utility.StringSliceIntersection(taskStatuses, evergreen.TaskFailureStatuses)) == 0 && isAborted
	if isAborted != b.Aborted {
		if err = b.SetAborted(isAborted); err != nil {
			return false, errors.Wrapf(err, "setting build '%s' as aborted", b.Id)
		}
	}

	event.LogBuildStateChangeEvent(b.Id, buildStatus)

	if evergreen.IsFinishedBuildStatus(buildStatus) {
		if err = b.MarkFinished(buildStatus, time.Now()); err != nil {
			return true, errors.Wrapf(err, "marking build as finished with status '%s'", buildStatus)
		}
		if err = updateMakespans(b, buildTasks); err != nil {
			return true, errors.Wrapf(err, "updating makespan information for '%s'", b.Id)
		}
	} else {
		if err = b.UpdateStatus(buildStatus); err != nil {
			return true, errors.Wrap(err, "updating build status")
		}
	}

	if err = updateBuildGithubStatus(b, buildTasks); err != nil {
		return true, errors.Wrap(err, "updating build GitHub status")
	}

	return true, nil
}

func getVersionStatus(builds []build.Build) string {
	// Check if no builds have started in the version.
	noStartedBuilds := true
	for _, b := range builds {
		if b.Status != evergreen.BuildCreated {
			noStartedBuilds = false
			break
		}
	}
	if noStartedBuilds {
		return evergreen.VersionCreated
	}

	// Check if builds are started but not finished.
	for _, b := range builds {
		if b.Activated && !evergreen.IsFinishedBuildStatus(b.Status) && !b.AllTasksBlocked {
			return evergreen.VersionStarted
		}
	}

	// Check if all builds are finished but have failures.
	for _, b := range builds {
		if b.Status == evergreen.BuildFailed || b.Aborted {
			return evergreen.VersionFailed
		}
	}

	return evergreen.VersionSucceeded
}

func updateVersionGithubStatus(v *Version, builds []build.Build) error {
	githubStatusBuilds := make([]build.Build, 0, len(builds))
	for _, b := range builds {
		if b.IsGithubCheck {
			b.Status = b.GithubCheckStatus
			githubStatusBuilds = append(githubStatusBuilds, b)
		}
	}
	if len(githubStatusBuilds) == 0 {
		return nil
	}

	githubBuildStatus := getVersionStatus(githubStatusBuilds)

	if evergreen.IsFinishedBuildStatus(githubBuildStatus) {
		event.LogVersionGithubCheckFinishedEvent(v.Id, githubBuildStatus)
	}

	return nil
}

// Update the status of the version based on its constituent builds
func updateVersionStatus(v *Version) (string, error) {
	builds, err := build.Find(build.ByVersion(v.Id).WithFields(build.ActivatedKey, build.StatusKey,
		build.IsGithubCheckKey, build.GithubCheckStatusKey, build.AbortedKey))
	if err != nil {
		return "", errors.Wrapf(err, "getting builds for version '%s'", v.Id)
	}

	// Regardless of whether the overall version status has changed, the Github status subset may have changed.
	if err = updateVersionGithubStatus(v, builds); err != nil {
		return "", errors.Wrap(err, "updating version GitHub status")
	}

	versionStatus := getVersionStatus(builds)
	if versionStatus == v.Status {
		return versionStatus, nil
	}

	// only need to check aborted if status has changed
	isAborted := false
	for _, b := range builds {
		if b.Aborted {
			isAborted = true
			break
		}
	}
	if isAborted != v.Aborted {
		if err = v.SetAborted(isAborted); err != nil {
			return "", errors.Wrapf(err, "setting version '%s' as aborted", v.Id)
		}
	}

	event.LogVersionStateChangeEvent(v.Id, versionStatus)

	if evergreen.IsFinishedVersionStatus(versionStatus) {
		if err = v.MarkFinished(versionStatus, time.Now()); err != nil {
			return "", errors.Wrapf(err, "marking version '%s' as finished with status '%s'", v.Id, versionStatus)
		}
	} else {
		if err = v.UpdateStatus(versionStatus); err != nil {
			return "", errors.Wrapf(err, "updating version '%s' with status '%s'", v.Id, versionStatus)
		}
	}

	return versionStatus, nil
}

func UpdatePatchStatus(p *patch.Patch, versionStatus string) error {
	patchStatus, err := evergreen.VersionStatusToPatchStatus(versionStatus)
	if err != nil {
		return errors.Wrapf(err, "getting patch status from version status '%s'", versionStatus)
	}

	if patchStatus == p.Status {
		return nil
	}

	event.LogPatchStateChangeEvent(p.Version, patchStatus)
	if evergreen.IsFinishedPatchStatus(patchStatus) {
		if err = p.MarkFinished(patchStatus, time.Now()); err != nil {
			return errors.Wrapf(err, "marking patch '%s' as finished with status '%s'", p.Id.Hex(), patchStatus)
		}
	} else {
		if err = p.UpdateStatus(patchStatus); err != nil {
			return errors.Wrapf(err, "updating patch '%s' with status '%s'", p.Id.Hex(), patchStatus)
		}
	}

	return nil
}

// UpdateBuildAndVersionStatusForTask updates the status of the task's build based on all the tasks in the build
// and the task's version based on all the builds in the version.
// Also update build and version Github statuses based on the subset of tasks and builds included in github checks
func UpdateBuildAndVersionStatusForTask(t *task.Task) error {
	taskBuild, err := build.FindOneId(t.BuildId)
	if err != nil {
		return errors.Wrapf(err, "getting build for task '%s'", t.Id)
	}
	if taskBuild == nil {
		return errors.Errorf("no build '%s' found for task '%s'", t.BuildId, t.Id)
	}
	buildStatusChanged, err := updateBuildStatus(taskBuild)
	if err != nil {
		return errors.Wrapf(err, "updating build '%s' status", taskBuild.Id)
	}
	// If no build has changed status, then we can assume the version and patch statuses have also stayed the same.
	if !buildStatusChanged {
		return nil
	}

	taskVersion, err := VersionFindOneId(t.Version)
	if err != nil {
		return errors.Wrapf(err, "getting version '%s' for task '%s'", t.Version, t.Id)
	}
	if taskVersion == nil {
		return errors.Errorf("no version '%s' found for task '%s'", t.Version, t.Id)
	}
	newVersionStatus, err := updateVersionStatus(taskVersion)
	if err != nil {
		return errors.Wrapf(err, "updating version '%s' status", taskVersion.Id)
	}

	if evergreen.IsPatchRequester(taskVersion.Requester) {
		p, err := patch.FindOneId(taskVersion.Id)
		if err != nil {
			return errors.Wrapf(err, "getting patch for version '%s'", taskVersion.Id)
		}
		if p == nil {
			return errors.Errorf("no patch found for version '%s'", taskVersion.Id)
		}
		if err = UpdatePatchStatus(p, newVersionStatus); err != nil {
			return errors.Wrapf(err, "updating patch '%s' status", p.Id.Hex())
		}
	}

	return nil
}

func UpdateVersionAndPatchStatusForBuilds(buildIds []string) error {
	if len(buildIds) == 0 {
		return nil
	}
	builds, err := build.Find(build.ByIds(buildIds))
	if err != nil {
		return errors.Wrapf(err, "fetching builds")
	}

	versionsToUpdate := make(map[string]string)
	for _, build := range builds {
		buildStatusChanged, err := updateBuildStatus(&build)
		if err != nil {
			return errors.Wrapf(err, "updating build '%s' status", build.Id)
		}
		// If no build has changed status, then we can assume the version and patch statuses have also stayed the same.
		if !buildStatusChanged {
			continue
		}

		versionsToUpdate[build.Version] = build.Id
	}
	for versionId, buildId := range versionsToUpdate {
		buildVersion, err := VersionFindOneId(versionId)
		if err != nil {
			return errors.Wrapf(err, "getting version '%s' for build '%s'", versionId, buildId)
		}
		if buildVersion == nil {
			return errors.Errorf("no version '%s' found for build '%s'", versionId, buildId)
		}
		newVersionStatus, err := updateVersionStatus(buildVersion)
		if err != nil {
			return errors.Wrapf(err, "updating version '%s' status", buildVersion.Id)
		}

		if evergreen.IsPatchRequester(buildVersion.Requester) {
			p, err := patch.FindOneId(buildVersion.Id)
			if err != nil {
				return errors.Wrapf(err, "getting patch for version '%s'", buildVersion.Id)
			}
			if p == nil {
				return errors.Errorf("no patch found for version '%s'", buildVersion.Id)
			}
			if err = UpdatePatchStatus(p, newVersionStatus); err != nil {
				return errors.Wrapf(err, "updating patch '%s' status", p.Id.Hex())
			}
		}
	}

	return nil
}

// MarkStart updates the task, build, version and if necessary, patch documents with the task start time
func MarkStart(t *task.Task, updates *StatusChanges) error {
	var err error

	startTime := time.Now().Round(time.Millisecond)

	if err = t.MarkStart(startTime); err != nil {
		return errors.WithStack(err)
	}
	event.LogTaskStarted(t.Id, t.Execution)

	// ensure the appropriate build is marked as started if necessary
	if err = build.TryMarkStarted(t.BuildId, startTime); err != nil {
		return errors.Wrap(err, "marking build started")
	}

	// ensure the appropriate version is marked as started if necessary
	if err = TryMarkVersionStarted(t.Version, startTime); err != nil {
		return errors.Wrap(err, "marking version started")
	}

	// if it's a patch, mark the patch as started if necessary
	if evergreen.IsPatchRequester(t.Requester) {
		err := patch.TryMarkStarted(t.Version, startTime)
		if err == nil {
			updates.PatchNewStatus = evergreen.PatchStarted

		} else if !adb.ResultsNotFound(err) {
			return errors.WithStack(err)
		}
	}

	if t.IsPartOfDisplay() {
		return UpdateDisplayTaskForTask(t)
	}

	return nil
}

// MarkHostTaskUndispatched marks a task as no longer dispatched to a host. If
// it's part of a display task, update the display task as necessary.
func MarkHostTaskUndispatched(t *task.Task) error {
	if err := t.MarkAsHostUndispatched(); err != nil {
		return errors.WithStack(err)
	}

	event.LogHostTaskUndispatched(t.Id, t.Execution, t.HostId)

	if t.IsPartOfDisplay() {
		return UpdateDisplayTaskForTask(t)
	}

	return nil
}

// MarkHostTaskDispatched marks a task as being dispatched to the host. If it's
// part of a display task, update the display task as necessary.
func MarkHostTaskDispatched(t *task.Task, h *host.Host) error {
	if err := t.MarkAsHostDispatched(h.Id, h.Distro.Id, h.AgentRevision, time.Now()); err != nil {
		return errors.Wrapf(err, "marking task '%s' as dispatched "+
			"on host '%s'", t.Id, h.Id)
	}

	event.LogHostTaskDispatched(t.Id, t.Execution, h.Id)

	if t.IsPartOfDisplay() {
		return UpdateDisplayTaskForTask(t)
	}

	return nil
}

func MarkOneTaskReset(t *task.Task) error {
	if t.DisplayOnly {
		if !t.ResetFailedWhenFinished {
			if err := MarkTasksReset(t.ExecutionTasks); err != nil {
				return errors.Wrap(err, "resetting execution tasks")
			}
		} else {
			failedExecTasks, err := task.FindWithFields(task.FailedTasksByIds(t.ExecutionTasks), task.IdKey)
			if err != nil {
				return errors.Wrap(err, "retrieving failed execution tasks")
			}
			failedExecTaskIds := []string{}
			for _, et := range failedExecTasks {
				failedExecTaskIds = append(failedExecTaskIds, et.Id)
			}
			if err := MarkTasksReset(failedExecTaskIds); err != nil {
				return errors.Wrap(err, "resetting failed execution tasks")
			}
		}
	}

	if err := t.Reset(); err != nil && !adb.ResultsNotFound(err) {
		return errors.Wrap(err, "resetting task in database")
	}

	if err := UpdateUnblockedDependencies(t); err != nil {
		return errors.Wrap(err, "clearing cached unattainable dependencies")
	}

	if err := t.MarkDependenciesFinished(false); err != nil {
		return errors.Wrap(err, "marking direct dependencies unfinished")
	}

	return nil
}

// MarkTasksReset resets many tasks by their IDs. For execution tasks, this also
// resets their parent display tasks.
func MarkTasksReset(taskIds []string) error {
	tasks, err := task.FindAll(db.Query(task.ByIds(taskIds)))
	if err != nil {
		return errors.WithStack(err)
	}
	tasks, err = task.AddParentDisplayTasks(tasks)
	if err != nil {
		return errors.WithStack(err)
	}

	if err = task.ResetTasks(tasks); err != nil {
		return errors.Wrap(err, "resetting tasks in database")
	}

	catcher := grip.NewBasicCatcher()
	for _, t := range tasks {
		catcher.Wrapf(UpdateUnblockedDependencies(&t), "clearing cached unattainable dependencies for task '%s'", t.Id)
		catcher.Wrapf(t.MarkDependenciesFinished(false), "marking direct dependencies unfinished for task '%s'", t.Id)
	}

	return catcher.Resolve()
}

// RestartFailedTasks attempts to restart failed tasks that started between 2 times
// It returns a slice of task IDs that were successfully restarted as well as a slice
// of task IDs that failed to restart
// opts.dryRun will return the tasks that will be restarted if sent true
// opts.red and opts.purple will only restart tasks that were failed due to the test
// or due to the system, respectively
func RestartFailedTasks(opts RestartOptions) (RestartResults, error) {
	results := RestartResults{}
	if !opts.IncludeTestFailed && !opts.IncludeSysFailed && !opts.IncludeSetupFailed {
		opts.IncludeTestFailed = true
		opts.IncludeSysFailed = true
		opts.IncludeSetupFailed = true
	}
	failureTypes := []string{}
	if opts.IncludeTestFailed {
		failureTypes = append(failureTypes, evergreen.CommandTypeTest)
	}
	if opts.IncludeSysFailed {
		failureTypes = append(failureTypes, evergreen.CommandTypeSystem)
	}
	if opts.IncludeSetupFailed {
		failureTypes = append(failureTypes, evergreen.CommandTypeSetup)
	}
	tasksToRestart, err := task.FindAll(db.Query(task.ByTimeStartedAndFailed(opts.StartTime, opts.EndTime, failureTypes)))
	if err != nil {
		return results, errors.WithStack(err)
	}
	tasksToRestart, err = task.AddParentDisplayTasks(tasksToRestart)
	if err != nil {
		return results, errors.WithStack(err)
	}

	type taskGroupAndBuild struct {
		Build     string
		TaskGroup string
	}
	// only need to check one task per task group / build combination, and once per display task
	taskGroupsToCheck := map[taskGroupAndBuild]string{}
	displayTasksToCheck := map[string]task.Task{}
	idsToRestart := []string{}
	for _, t := range tasksToRestart {
		if t.IsPartOfDisplay() {
			dt, err := t.GetDisplayTask()
			if err != nil {
				return results, errors.Wrap(err, "getting display task")
			}
			displayTasksToCheck[t.DisplayTask.Id] = *dt
		} else if t.DisplayOnly {
			displayTasksToCheck[t.Id] = t
		} else if t.IsPartOfSingleHostTaskGroup() {
			taskGroupsToCheck[taskGroupAndBuild{
				TaskGroup: t.TaskGroup,
				Build:     t.BuildId,
			}] = t.Id
		} else {
			idsToRestart = append(idsToRestart, t.Id)
		}
	}

	for id, dt := range displayTasksToCheck {
		if dt.IsFinished() {
			idsToRestart = append(idsToRestart, id)
		} else {
			if err = dt.SetResetWhenFinished(); err != nil {
				return results, errors.Wrapf(err, "marking display task '%s' for reset", id)
			}
		}
	}
	for _, tg := range taskGroupsToCheck {
		idsToRestart = append(idsToRestart, tg)
	}

	// if this is a dry run, immediately return the tasks found
	if opts.DryRun {
		results.ItemsRestarted = idsToRestart
		return results, nil
	}

	return doRestartFailedTasks(idsToRestart, opts.User, results), nil
}

func doRestartFailedTasks(tasks []string, user string, results RestartResults) RestartResults {
	var tasksErrored []string

	for _, id := range tasks {
		if err := TryResetTask(id, user, evergreen.RESTV2Package, nil); err != nil {
			tasksErrored = append(tasksErrored, id)
			grip.Error(message.Fields{
				"task":    id,
				"status":  "failed",
				"message": "error restarting task",
				"error":   err.Error(),
			})
		} else {
			results.ItemsRestarted = append(results.ItemsRestarted, id)
		}
	}
	results.ItemsErrored = tasksErrored

	return results
}

// CheckAndBlockSingleHostTaskGroup blocks a single-host task group if the given
// task is part of one and has finished (or is currently finishing) with a
// failed status. This is a best-effort attempt, so if it fails, it will just
// log the error.
func CheckAndBlockSingleHostTaskGroup(t *task.Task, status string) {
	if !t.IsPartOfSingleHostTaskGroup() || status == evergreen.TaskSucceeded {
		return
	}

	// For a single-host task group, block and dequeue later tasks in that group.
	if err := BlockTaskGroupTasks(t.Id); err != nil {
		grip.Error(message.WrapError(err, message.Fields{
			"message": "problem blocking task group tasks",
			"task_id": t.Id,
		}))
		return
	}

	grip.Debug(message.Fields{
		"message": "blocked task group tasks for task",
		"task_id": t.Id,
	})
}

// ClearAndResetStrandedContainerTask clears the container task dispatched to a
// pod. It also resets the task so that the current task execution is marked as
// finished and, if necessary, a new execution is created to restart the task.
// TODO (PM-2618): should probably block single-container task groups once
// they're supported.
func ClearAndResetStrandedContainerTask(p *pod.Pod) error {
	runningTaskID := p.TaskRuntimeInfo.RunningTaskID
	runningTaskExecution := p.TaskRuntimeInfo.RunningTaskExecution
	if runningTaskID == "" {
		return nil
	}

	// Note that clearing the pod and resetting the task are not atomic
	// operations, so it's possible for the pod's running task to be cleared but
	// the stranded task fails to reset.
	// In this case, there are other cleanup jobs to detect when a task is
	// stranded on a terminated pod.
	if err := p.ClearRunningTask(); err != nil {
		return errors.Wrapf(err, "clearing running task '%s' execution %d from pod '%s'", runningTaskID, runningTaskExecution, p.ID)
	}

	t, err := task.FindOneIdAndExecution(runningTaskID, runningTaskExecution)
	if err != nil {
		return errors.Wrapf(err, "finding running task '%s' execution %d from pod '%s'", runningTaskID, runningTaskExecution, p.ID)
	}
	if t == nil {
		return nil
	}

	if t.Archived {
		grip.Warning(message.Fields{
			"message":   "stranded container task has already been archived, refusing to fix it",
			"task":      t.Id,
			"execution": t.Execution,
			"status":    t.Status,
		})
		return nil
	}

	if err := resetSystemFailedTask(t, evergreen.TaskDescriptionStranded); err != nil {
		return errors.Wrapf(err, "resetting stranded task '%s'", t.Id)
	}

	grip.Info(message.Fields{
		"message":            "successfully fixed stranded container task",
		"task":               t.Id,
		"execution":          t.Execution,
		"execution_platform": t.ExecutionPlatform,
	})

	return nil
}

// ClearAndResetStrandedHostTask clears the host task dispatched to the host due
// to being stranded on a bad host (e.g. one that has been terminated). It also
// marks the current task execution as finished and, if possible, a new
// execution is created to restart the task.
func ClearAndResetStrandedHostTask(h *host.Host) error {
	if h.RunningTask == "" {
		return nil
	}

	t, err := task.FindOneId(h.RunningTask)
	if err != nil {
		return errors.Wrapf(err, "finding running task '%s' from host '%s'", h.RunningTask, h.Id)
	} else if t == nil {
		return nil
	}

	CheckAndBlockSingleHostTaskGroup(t, t.Status)

	if err = h.ClearRunningTask(); err != nil {
		return errors.Wrapf(err, "clearing running task from host '%s'", h.Id)
	}

	if err := resetSystemFailedTask(t, evergreen.TaskDescriptionStranded); err != nil {
		return errors.Wrapf(err, "resetting stranded task '%s'", t.Id)
	}

	grip.Info(message.Fields{
		"message":            "successfully fixed stranded host task",
		"task":               t.Id,
		"execution":          t.Execution,
		"execution_platform": t.ExecutionPlatform,
	})

	return nil
}

// ResetStaleTask fixes a task that has exceeded the heartbeat timeout by
// marking the current task execution as finished and, if possible, a new
// execution is created to restart the task.
func ResetStaleTask(t *task.Task) error {
	CheckAndBlockSingleHostTaskGroup(t, t.Status)

	if err := resetSystemFailedTask(t, evergreen.TaskDescriptionHeartbeat); err != nil {
		return errors.Wrap(err, "resetting task")
	}

	grip.Info(message.Fields{
		"message":            "successfully fixed stale heartbeat task",
		"task":               t.Id,
		"execution":          t.Execution,
		"execution_platform": t.ExecutionPlatform,
	})

	return nil
}

// resetSystemFailedTask resets a task that has encountered a system failure
// such as being stranded on a terminated host/container or failing to send a
// heartbeat.
func resetSystemFailedTask(t *task.Task, description string) error {
	if t.IsFinished() {
		return nil
	}

	if err := t.MarkSystemFailed(description); err != nil {
		return errors.Wrap(err, "marking task as system failed")
	}

	if time.Since(t.ActivatedTime) > task.UnschedulableThreshold {
		// If the task has already exceeded the unschedulable threshold, we
		// don't want to restart it, so just mark it as finished.
		if t.DisplayOnly {
			execTasks, err := task.FindAll(db.Query(task.ByIds(t.ExecutionTasks)))
			if err != nil {
				return errors.Wrap(err, "finding execution tasks")
			}
			for _, execTask := range execTasks {
				if !evergreen.IsFinishedTaskStatus(execTask.Status) {
					if err := MarkEnd(&execTask, evergreen.MonitorPackage, time.Now(), &t.Details, false); err != nil {
						return errors.Wrap(err, "marking execution task as ended")
					}
				}
			}
		}
		return errors.WithStack(MarkEnd(t, evergreen.MonitorPackage, time.Now(), &t.Details, false))
	}

	return errors.Wrap(ResetTaskOrDisplayTask(t, evergreen.User, evergreen.MonitorPackage, true, &t.Details), "resetting task")
}

// ResetTaskOrDisplayTask is a wrapper for TryResetTask that handles execution and display tasks that are restarted
// from sources separate from marking the task finished. If an execution task, attempts to restart the display task instead.
// Marks display tasks as reset when finished and then check if it can be reset immediately.
func ResetTaskOrDisplayTask(t *task.Task, user, origin string, failedOnly bool, detail *apimodels.TaskEndDetail) error {
	taskToReset := *t
	if taskToReset.IsPartOfDisplay() { // if given an execution task, attempt to restart the full display task
		dt, err := taskToReset.GetDisplayTask()
		if err != nil {
			return errors.Wrap(err, "getting display task")
		}
		if dt != nil {
			taskToReset = *dt
		}
	}
	if taskToReset.DisplayOnly {
		if failedOnly {
			if err := taskToReset.SetResetFailedWhenFinished(); err != nil {
				return errors.Wrap(err, "marking display task for reset")
			}
		} else {
			if err := taskToReset.SetResetWhenFinished(); err != nil {
				return errors.Wrap(err, "marking display task for reset")
			}
		}
		return errors.Wrap(checkResetDisplayTask(&taskToReset), "checking display task reset")
	}

	return errors.Wrap(TryResetTask(t.Id, user, origin, detail), "resetting task")
}

// UpdateDisplayTaskForTask updates the status of the given execution task's display task
func UpdateDisplayTaskForTask(t *task.Task) error {
	if !t.IsPartOfDisplay() {
		return errors.Errorf("task '%s' is not an execution task", t.Id)
	}
	dt, err := t.GetDisplayTask()
	if err != nil {
		return errors.Wrap(err, "getting display task for task")
	}
	if dt == nil {
		grip.Error(message.Fields{
			"message":         "task may hold a display task that doesn't exist",
			"task_id":         t.Id,
			"display_task_id": t.DisplayTaskId,
		})
		return errors.Errorf("display task not found for task '%s'", t.Id)
	}
	if !dt.DisplayOnly {
		return errors.Errorf("task '%s' is not a display task", dt.Id)
	}

	var timeTaken time.Duration
	var statusTask task.Task
	execTasks, err := task.Find(task.ByIds(dt.ExecutionTasks))
	if err != nil {
		return errors.Wrap(err, "retrieving execution tasks")
	}
	hasFinishedTasks := false
	hasTasksToRun := false
	startTime := time.Unix(1<<62, 0)
	endTime := utility.ZeroTime
	for _, execTask := range execTasks {
		// if any of the execution tasks are scheduled, the display task is too
		if execTask.Activated {
			dt.Activated = true
			if utility.IsZeroTime(dt.ActivatedTime) {
				dt.ActivatedTime = time.Now()
			}
		}
		if execTask.IsFinished() {
			hasFinishedTasks = true
			// Need to consider tasks that have been dispatched since the last exec task finished.
		} else if (execTask.IsDispatchable() || execTask.IsAbortable()) && !execTask.Blocked() {
			hasTasksToRun = true
		}

		// add up the duration of the execution tasks as the cumulative time taken
		timeTaken += execTask.TimeTaken

		// set the start/end time of the display task as the earliest/latest task
		if !utility.IsZeroTime(execTask.StartTime) && execTask.StartTime.Before(startTime) {
			startTime = execTask.StartTime
		}
		if execTask.FinishTime.After(endTime) {
			endTime = execTask.FinishTime
		}
	}

	sort.Sort(task.ByPriority(execTasks))
	statusTask = execTasks[0]
	if hasFinishedTasks && hasTasksToRun {
		// if an unblocked display task has a mix of finished and unfinished tasks, the display task is still
		// "started" even if there aren't currently running tasks
		statusTask.Status = evergreen.TaskStarted
		statusTask.Details = apimodels.TaskEndDetail{}
	}

	update := bson.M{
		task.StatusKey:        statusTask.Status,
		task.ActivatedKey:     dt.Activated,
		task.ActivatedTimeKey: dt.ActivatedTime,
		task.TimeTakenKey:     timeTaken,
		task.DetailsKey:       statusTask.Details,
	}

	if startTime != time.Unix(1<<62, 0) {
		update[task.StartTimeKey] = startTime
	}
	if endTime != utility.ZeroTime && !hasTasksToRun {
		update[task.FinishTimeKey] = endTime
	}

	// refresh task status from db in case of race
	taskWithStatus, err := task.FindOneIdWithFields(dt.Id, task.StatusKey)
	if err != nil {
		return errors.Wrapf(err, "refreshing task '%s'", dt.Id)
	}
	if taskWithStatus == nil {
		return errors.Errorf("task '%s' not found", dt.Id)
	}
	wasFinished := taskWithStatus.IsFinished()
	err = task.UpdateOne(
		bson.M{
			task.IdKey: dt.Id,
		},
		bson.M{
			"$set": update,
		})
	if err != nil {
		return errors.Wrap(err, "updating display task")
	}
	dt.Status = statusTask.Status
	dt.Details = statusTask.Details
	dt.TimeTaken = timeTaken
	if !wasFinished && dt.IsFinished() {
		event.LogTaskFinished(dt.Id, dt.Execution, "", dt.GetDisplayStatus())
		grip.Info(message.Fields{
			"message":   "display task finished",
			"task_id":   dt.Id,
			"status":    dt.Status,
			"operation": "UpdateDisplayTaskForTask",
		})
	}
	return nil
}

// checkResetSingleHostTaskGroup attempts to reset all tasks that are part of
// the same single-host task group as t once all tasks in the task group are
// finished running.
func checkResetSingleHostTaskGroup(t *task.Task, caller string) error {
	if !t.IsPartOfSingleHostTaskGroup() {
		return nil
	}
	tasks, err := task.FindTaskGroupFromBuild(t.BuildId, t.TaskGroup)
	if err != nil {
		return errors.Wrapf(err, "getting task group for task '%s'", t.Id)
	}
	if len(tasks) == 0 {
		return errors.Errorf("no tasks in task group '%s' for task '%s'", t.TaskGroup, t.Id)
	}
	shouldReset := false
	for _, tgTask := range tasks {
		if tgTask.ResetWhenFinished {
			shouldReset = true
		}
		if !tgTask.IsFinished() && !tgTask.Blocked() && tgTask.Activated { // task in group still needs to run
			return nil
		}
	}

	if !shouldReset { // no task in task group has requested a reset
		return nil
	}

	return errors.Wrap(resetManyTasks(tasks, caller), "resetting task group tasks")
}

// checkResetDisplayTask attempts to reset all tasks that are under the same
// parent display task as t once all tasks under the display task are finished
// running.
func checkResetDisplayTask(t *task.Task) error {
	if !t.ResetWhenFinished && !t.ResetFailedWhenFinished {
		return nil
	}
	execTasks, err := task.Find(task.ByIds(t.ExecutionTasks))
	if err != nil {
		return errors.Wrapf(err, "getting execution tasks for display task '%s'", t.Id)
	}
	for _, execTask := range execTasks {
		if !execTask.IsFinished() && !execTask.Blocked() && execTask.Activated {
			return nil // all tasks not finished
		}
	}
	details := &t.Details
	// Assign task end details to indicate system failure if we receive no valid details
	if details.IsEmpty() && !t.IsFinished() {
		details = &apimodels.TaskEndDetail{
			Type:   evergreen.CommandTypeSystem,
			Status: evergreen.TaskFailed,
		}
	}
	return errors.Wrap(TryResetTask(t.Id, evergreen.User, evergreen.User, details), "resetting display task")
}

// MarkUnallocatableContainerTasksSystemFailed marks any container task within
// the candidate task IDs that needs to re-allocate a container but has used up
// all of its container allocation attempts as finished due to system failure.
func MarkUnallocatableContainerTasksSystemFailed(candidateTaskIDs []string) error {
	var unallocatableTasks []task.Task
	for _, taskID := range candidateTaskIDs {
		tsk, err := task.FindOneId(taskID)
		if err != nil {
			return errors.Wrapf(err, "finding task '%s'", taskID)
		}
		if tsk == nil {
			continue
		}
		if !tsk.IsContainerTask() {
			continue
		}
		if !tsk.ContainerAllocated {
			continue
		}
		if tsk.RemainingContainerAllocationAttempts() > 0 {
			continue
		}

		unallocatableTasks = append(unallocatableTasks, *tsk)
	}

	catcher := grip.NewBasicCatcher()
	for _, tsk := range unallocatableTasks {
		details := apimodels.TaskEndDetail{
			Status:      evergreen.TaskFailed,
			Type:        evergreen.CommandTypeSystem,
			Description: evergreen.TaskDescriptionContainerUnallocatable,
		}
		if err := MarkEnd(&tsk, evergreen.APIServerTaskActivator, time.Now(), &details, false); err != nil {
			catcher.Wrapf(err, "marking task '%s' as a failure due to inability to allocate", tsk.Id)
		}
	}

	return catcher.Resolve()
}

package model

import (
	"time"

	"github.com/evergreen-ci/evergreen/model/build"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/patch"
	"github.com/evergreen-ci/evergreen/model/task"
)

// TaskCreationInfo contains the needed parameters to construct new builds and tasks for a given version.
type TaskCreationInfo struct {
	Version             *Version
	Project             *Project
	ProjectRef          *ProjectRef
	BuildVariant        *BuildVariant
	Build               *build.Build
	Pairs               TaskVariantPairs
	BuildVariantName    string
	TaskIDs             TaskIdConfig            // Pre-generated IDs for the tasks to be created
	ActivateBuild       bool                    // True if the build should be scheduled
	ActivationInfo      specificActivationInfo  // Indicates if the task has a specific activation or is a stepback task
	TasksInBuild        []task.Task             // The set of task names that already exist for the given build, including display tasks
	TaskNames           []string                // Names of tasks to create (used in patches). Will create all if nil
	DisplayNames        []string                // Names of display tasks to create (used in patches). Will create all if nil
	GeneratedBy         string                  // ID of the task that generated this build
	SourceRev           string                  // Githash of the revision that triggered this build
	DefinitionID        string                  // Definition ID of the trigger used to create this build
	Aliases             ProjectAliases          // Project aliases to use to filter tasks created
	DistroAliases       distro.AliasLookupTable // Map of distro aliases to names of distros
	TaskCreateTime      time.Time               // Create time of tasks in the build
	GithubChecksAliases ProjectAliases          // Project aliases to use to filter tasks to count towards the github checks, if any
	SyncAtEndOpts       patch.SyncAtEndOptions  // Describes how tasks should sync upon the end of a task
}

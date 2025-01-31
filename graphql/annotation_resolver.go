package graphql

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"context"
	"fmt"

	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/task"
	restModel "github.com/evergreen-ci/evergreen/rest/model"
)

func (r *annotationResolver) WebhookConfigured(ctx context.Context, obj *restModel.APITaskAnnotation) (bool, error) {
	t, err := task.FindOneId(*obj.TaskId)
	if err != nil {
		return false, InternalServerError.Send(ctx, fmt.Sprintf("error finding task: %s", err.Error()))
	}
	if t == nil {
		return false, ResourceNotFound.Send(ctx, "error finding task for the task annotation")
	}
	_, ok, _ := model.IsWebhookConfigured(t.Project, t.Version)
	return ok, nil
}

// Annotation returns AnnotationResolver implementation.
func (r *Resolver) Annotation() AnnotationResolver { return &annotationResolver{r} }

type annotationResolver struct{ *Resolver }

package graphql

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"context"
	"sort"

	restModel "github.com/evergreen-ci/evergreen/rest/model"
)

func (r *projectVarsResolver) AdminOnlyVars(ctx context.Context, obj *restModel.APIProjectVars) ([]string, error) {
	res := []string{}
	for varAlias, isAdminOnly := range obj.AdminOnlyVars {
		if isAdminOnly {
			res = append(res, varAlias)
		}
	}
	sort.Strings(res)
	return res, nil
}

func (r *projectVarsResolver) PrivateVars(ctx context.Context, obj *restModel.APIProjectVars) ([]string, error) {
	res := []string{}
	for privateAlias, isPrivate := range obj.PrivateVars {
		if isPrivate {
			res = append(res, privateAlias)
		}
	}
	sort.Strings(res)
	return res, nil
}

// ProjectVars returns ProjectVarsResolver implementation.
func (r *Resolver) ProjectVars() ProjectVarsResolver { return &projectVarsResolver{r} }

type projectVarsResolver struct{ *Resolver }

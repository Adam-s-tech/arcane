package userctx

import (
	"context"

	"github.com/getarcaneapp/arcane/types/v2/user"
)

// CurrentUserFromContext retrieves the actor for an authenticated request.
func CurrentUserFromContext(ctx context.Context) (*user.Actor, bool) {
	value := ctx.Value(user.CurrentUserContextKey{})
	if actor, ok := value.(*user.Actor); ok {
		return actor, true
	}
	provider, ok := value.(user.ActorProvider)
	if !ok {
		return nil, false
	}
	return provider.Actor(), true
}

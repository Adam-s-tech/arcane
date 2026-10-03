package user

import (
	"context"

	"github.com/getarcaneapp/arcane/types/v2/user"
)

// CurrentUserFromContext retrieves the authenticated persistence model.
func CurrentUserFromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(user.CurrentUserContextKey{}).(*User)
	return u, ok
}

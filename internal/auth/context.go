package auth

import "context"

// ctxKey is the private key type for the signed-in user.
type ctxKey struct{}

// withUser attaches the signed-in user to a request context.
func withUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the signed-in user, if any. Handlers use it to show who is
// signed in; it is empty when authentication is not configured.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

package e2e

// ptrOf returns a pointer to the given value
func ptrOf[T any](t T) *T {
	return &t
}

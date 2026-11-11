package harness

// Default is the registry the daemon serves.
func Default() *Registry {
	return New(Claude())
}

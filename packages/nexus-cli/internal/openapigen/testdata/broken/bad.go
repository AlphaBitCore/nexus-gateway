package broken

// F references an undefined symbol so the package fails type-checking,
// exercising openapigen's fail-loud-on-type-error path.
func F() int { return notDefined }

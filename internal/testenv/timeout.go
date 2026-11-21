package testenv

import "time"

// HangTimeout bounds fixture work without making scheduler speed an assertion.
const HangTimeout = 2 * time.Minute

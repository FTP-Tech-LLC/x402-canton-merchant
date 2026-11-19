package echo

import echolib "github.com/labstack/echo/v4"

// ResetPreWarned lets a test see the once-per-process e.Pre warning again.
func ResetPreWarned() { preWarned.Store(false) }

// HasOwnRoute exposes the own-route rule to the tests.
func HasOwnRoute(c echolib.Context) bool { return hasOwnRoute(c) }

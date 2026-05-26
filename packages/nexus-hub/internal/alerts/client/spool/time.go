package spool

import "time"

func timeNowUnixNano() int64 { return time.Now().UnixNano() }

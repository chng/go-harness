package action

import "time"

func timeSeconds(s int) time.Duration { return time.Duration(s) * time.Second }

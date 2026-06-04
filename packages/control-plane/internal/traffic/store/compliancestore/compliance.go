package compliancestore

import (
	"time"
)

// TimePeriod holds a closed [Start, End] time interval.
type TimePeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// LabelCount is a generic label+count pair for top-N lists.
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

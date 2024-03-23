package cli

import "time"

type PeriodCounter struct {
	periodDuration  time.Duration
	currentDuration time.Duration
}

func NewPeriodCounter(periodDuration time.Duration) *PeriodCounter {
	return &PeriodCounter{
		periodDuration:  periodDuration,
		currentDuration: 0,
	}
}

func (c *PeriodCounter) AddDuration(duration time.Duration) bool {
	c.currentDuration += duration
	return c.IsTimeForAction()
}

func (c *PeriodCounter) ResetCounter() {
	c.currentDuration = 0
}

func (c *PeriodCounter) IsTimeForAction() bool {
	return c.currentDuration >= c.periodDuration
}

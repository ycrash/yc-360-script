package utils

import (
	"testing"
	"time"
)

type counterTestStep struct {
	interval time.Duration
	isTime   bool
}

func TestPeriodCounterOneMinute(t *testing.T) {
	intervalOneMinute := time.Minute * 1
	testSteps := []counterTestStep{
		{ // 1
			interval: intervalOneMinute,
			isTime:   false,
		},
		{ // 2
			interval: intervalOneMinute,
			isTime:   false,
		},
		{ // 3
			interval: intervalOneMinute,
			isTime:   false,
		},
		{ // 4
			interval: intervalOneMinute,
			isTime:   false,
		},
		{ // 5
			interval: intervalOneMinute,
			isTime:   true,
		},
	}
	counter := NewPeriodCounter(time.Minute * 5)
	DoTestPeriodCounter(t, counter, testSteps)
}

func TestPeriodCounterTwoMinute(t *testing.T) {
	intervalTwoMinutes := time.Minute * 2
	testSteps := []counterTestStep{
		{ // 2
			interval: intervalTwoMinutes,
			isTime:   false,
		},
		{ // 4
			interval: intervalTwoMinutes,
			isTime:   false,
		},
		{ // 6
			interval: intervalTwoMinutes,
			isTime:   true,
		},
	}
	counter := NewPeriodCounter(time.Minute * 5)
	DoTestPeriodCounter(t, counter, testSteps)
}

func TestPeriodCounterThreeMinute(t *testing.T) {
	intervalThreeMinutes := time.Minute * 3
	testSteps := []counterTestStep{
		{ // 3
			interval: intervalThreeMinutes,
			isTime:   false,
		},
		{ // 6
			interval: intervalThreeMinutes,
			isTime:   true,
		},
	}
	counter := NewPeriodCounter(time.Minute * 5)
	DoTestPeriodCounter(t, counter, testSteps)
}

func TestPeriodCounterFiveMinute(t *testing.T) {
	intervalFiveMinutes := time.Minute * 5
	testSteps := []counterTestStep{
		{ // 5
			interval: intervalFiveMinutes,
			isTime:   true,
		},
	}
	counter := NewPeriodCounter(time.Minute * 5)
	DoTestPeriodCounter(t, counter, testSteps)
}

func TestPeriodCounterTenMinute(t *testing.T) {
	intervalTenMinutes := time.Minute * 10
	testSteps := []counterTestStep{
		{ // 10
			interval: intervalTenMinutes,
			isTime:   true,
		},
	}
	counter := NewPeriodCounter(time.Minute * 5)
	DoTestPeriodCounter(t, counter, testSteps)
}

func DoTestPeriodCounter(t *testing.T, counter *PeriodCounter, steps []counterTestStep) {
	for _, step := range steps {
		isTime := counter.AddDuration(step.interval)
		isTime2 := counter.IsTimeForAction()
		if isTime2 != isTime {
			t.Error("AddDuration result is not relevant to IsTimeForAction")
		}
		if isTime2 != step.isTime {
			t.Error("IsTimeForAction result is not as expected")
		}
	}
}

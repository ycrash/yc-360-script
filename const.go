package shell

import (
	"time"
)

// ------------------------------------------------------------------------------
//  Customer specific Properties
// ------------------------------------------------------------------------------

// ------------------------------------------------------------------------------
//
//	Generic Properties
//
// ------------------------------------------------------------------------------
var (
	SCRIPT_VERSION      = "yc_agent_2.19"
	SCRIPT_SPAN         = 120 // How long the whole script should take. Default=240
	JAVACORE_INTERVAL   = 30  // How often javacores should be taken. Default=30
	TOP_INTERVAL        = 60  // How often top data should be taken. Default=60
	TOP_DASH_H_INTERVAL = 5   // How often top dash H data should be taken. Default=5
	VMSTAT_INTERVAL     = 5   // How often vmstat data should be taken. Default=5
)

// ------------------------------------------------------------------------------
//  * All values are in seconds.
//  * All the 'INTERVAL' values should divide into the 'SCRIPT_SPAN' by a whole
//    integer to obtain expected results.
//  * Setting any 'INTERVAL' too low (especially JAVACORE) can result in data
//    that may not be useful towards resolving the issue.  This becomes a problem
//    when the process of collecting data obscures the real issue.
// ------------------------------------------------------------------------------

func NowString() string {
	return time.Now().Format("Mon Jan 2 15:04:05 MST 2006 ")
}

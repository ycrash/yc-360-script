package shell

import "time"

// ------------------------------------------------------------------------------
//  Generic Properties
// ------------------------------------------------------------------------------
var (
	SCRIPT_VERSION      = "2019_07_04"
	SCRIPT_SPAN         = 120 // How long the whole script should take. Default=240
	JAVACORE_INTERVAL   = 30  // How often javacores should be taken. Default=30
	TOP_INTERVAL        = 60  // How often top data should be taken. Default=60
	TOP_DASH_H_INTERVAL = 5   // How often top dash H data should be taken. Default=5
	VMSTAT_INTERVAL     = 5   // How often vmstat data should be taken. Default=5
)

func NowString() string {
	return time.Now().Format("Mon Jan 2 15:04:05 MST 2006 ")
}

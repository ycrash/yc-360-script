package ondemand

import (
	"fmt"
	"testing"
)

func TestPrintResult(t *testing.T) {
	s := `{"dashboardReportURL":"https://gceasy.io/yc-report.jsp?ou=testCompany&de=1721802&app=yc&ts=2020-12-30T09-19-13","threadMetricsURL":"https://gceasy.io/yc-reader?ou=testCompany&de=1721802&app=yc&ts=2020-12-30T09-19-13&dt=td&apiKey=testCompany@00000000-0000-0000-0000-000000000000","gcMetricsURL":"https://gceasy.io/yc-reader?ou=testCompany&de=1721802&app=yc&ts=2020-12-30T09-19-13&dt=gc&apiKey=testCompany@00000000-0000-0000-0000-000000000000"}`
	fmt.Println(printResult(true, "2m10s", []byte(s)))
}

func TestFormat(t *testing.T) {
	s := "https://gceasy.io/yc-reader?ou=testCompany&de=1721802&app=yc&ts=2020-12-30T09-19-13&dt=td&apiKey=testCompany@00000000-0000-0000-0000-000000000000"
	t.Log("\n", format(10, s))
}

package main

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/pterm/pterm"
	"shell/config"
)

var col1 = []string{
	"Port",
	"Status",
	"RunTime",
}

func printResult(success bool, runtime string, resp []byte) string {
	m := make(map[string]string)
	err := json.Unmarshal(resp, &m)
	if err != nil {
		return ""
	}
	col2 := make([]string, len(col1))
	col2[0] = strconv.Itoa(config.GlobalConfig.Port)
	if success {
		col2[1] = "success"
	} else {
		col2[1] = "fail"
	}
	col2[2] = runtime
	d := pterm.TableData{}
	for i, s := range col1 {
		if s != "DashboardReportURL" {
			d = append(d, []string{s, col2[i]})
		} else {
			d = append(d, []string{pterm.LightGreen(s), pterm.LightGreen(col2[i])})
		}
	}
	var sortKeys []string
	for k := range m {
		sortKeys = append(sortKeys, k)
	}
	sort.Strings(sortKeys)
	for _, s := range sortKeys {
		if s != "dashboardReportURL" {
			d = append(d, []string{s, m[s]})
		} else {
			d = append(d, []string{pterm.LightGreen(s), pterm.LightGreen(m[s])})
		}
	}
	srender, err := pterm.DefaultTable.WithHasHeader(false).WithData(d).Srender()
	if err != nil {
		return ""
	}
	return pterm.DefaultBox.WithRightPadding(1).WithBottomPadding(0).Sprintln(srender)
}

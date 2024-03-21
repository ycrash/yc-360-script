package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"shell/internal/config"

	"github.com/pterm/pterm"
)

var col1 = []string{
	"Port",
	"Status",
	"RunTime",
}

func printResult(success bool, runtime string, resp []byte) (reportUrl string, result string) {
	m := make(map[string]string)
	err := json.Unmarshal(resp, &m)
	if err != nil {
		return
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
		d = append(d, []string{s, col2[i]})
	}
	var sortKeys []string
	var maxKeyWidth int
	for k := range m {
		sortKeys = append(sortKeys, k)
		if maxKeyWidth == 0 {
			maxKeyWidth = len(k)
		} else if maxKeyWidth < len(k) {
			maxKeyWidth = len(k)
		}
	}
	sort.Strings(sortKeys)
	for _, s := range sortKeys {
		c := m[s]
		if s != "dashboardReportURL" {
			d = append(d, []string{s, c})
		} else {
			reportUrl = c
			d = append(d, []string{pterm.LightGreen(s), c})
		}
	}

	srender, err := TablePrinter{
		TablePrinter: pterm.DefaultTable.WithHasHeader(false).WithData(d),
		Width:        pterm.GetTerminalWidth(),
	}.Srender()
	if err != nil {
		return
	}
	result = pterm.DefaultBox.WithRightPadding(1).WithBottomPadding(0).Sprint(srender)
	return
}

func format(width int, s string) (result []string) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return nil
	}
	if len(s) <= width {
		return nil
	}
	result = append(result, s[:width])
	n := 1
	r := len(s) - width*n
	for r > 0 {
		if r > width {
			result = append(result, s[width*n:width*(n+1)])
		} else {
			result = append(result, s[width*n:])
			break
		}
		n += 1
		r = len(s) - width*n
	}
	return
}

type TablePrinter struct {
	*pterm.TablePrinter
	Width int
}

func (p TablePrinter) Srender() (string, error) {
	if p.Style == nil {
		p.Style = pterm.NewStyle()
	}
	if p.SeparatorStyle == nil {
		p.SeparatorStyle = pterm.NewStyle()
	}
	if p.HeaderStyle == nil {
		p.HeaderStyle = pterm.NewStyle()
	}

	var ret string
	maxColumnWidth := make(map[int]int)

	for _, row := range p.Data {
		for ci, column := range row {
			columnLength := len(pterm.RemoveColorFromString(column))
			if columnLength > maxColumnWidth[ci] {
				maxColumnWidth[ci] = columnLength
			}
		}
	}

	left := p.Width - maxColumnWidth[0] - 7
	if left < 1 {
		left = 10
	}
	if maxColumnWidth[1] > left {
		data := pterm.TableData{}
		for _, row := range p.Data {
			column := row[1]
			cs := pterm.RemoveColorFromString(column)
			columnLength := len(cs)
			if columnLength > left {
				cols := format(left, cs)
				for i, col := range cols {
					if i == 0 {
						data = append(data, []string{row[0], col})
					} else {
						data = append(data, []string{"", col})
					}
				}
			} else {
				data = append(data, row)
			}
		}
		maxColumnWidth[1] = left
		p.Data = data
	}

	for ri, row := range p.Data {
		for ci, column := range row {
			columnLength := len(pterm.RemoveColorFromString(column))
			columnString := column + strings.Repeat(" ", maxColumnWidth[ci]-columnLength)

			if ci != len(row) && ci != 0 {
				ret += p.Style.Sprint(p.SeparatorStyle.Sprint(p.Separator))
			}

			if p.HasHeader && ri == 0 {
				ret += p.Style.Sprint(p.HeaderStyle.Sprint(columnString))
			} else {
				ret += p.Style.Sprint(columnString)
			}
		}

		ret += "\n"
	}

	ret = strings.TrimSuffix(ret, "\n")
	return ret, nil
}

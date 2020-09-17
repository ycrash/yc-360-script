package shell

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shell/config"
)

type Server struct {
	*http.Server
	ProcessPids func(pids []int) (err error)
}

type Req struct {
	Key     string
	Actions []string
}

type Resp struct {
	Code int
	Msg  string
}

func (s *Server) Action(writer http.ResponseWriter, request *http.Request) {
	encoder := json.NewEncoder(writer)
	resp := &Resp{}

	defer func() {
		encoder.Encode(resp)
	}()

	decoder := json.NewDecoder(request.Body)
	req := &Req{}
	err := decoder.Decode(req)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}

	if config.GlobalConfig.ApiKey != req.Key {
		resp.Code = -1
		resp.Msg = "invalid key passed"
		return
	}

	pids, err := parseActions(req.Actions)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}

	go s.ProcessPids(pids)
}

func parseActions(actions []string) (pids []int, err error) {
	for _, s := range actions {
		if strings.HasPrefix(s, "capture ") {
			ss := strings.Split(s, " ")
			if len(ss) == 2 {
				id := ss[1]
				pid, err := strconv.Atoi(id)
				// "actions": ["capture buggyApp.jar"]
				if err != nil {
					pids, err := GetProcessIds(config.ProcessTokens{config.ProcessToken(id)})
					if err != nil {
						continue
					}
					if len(pids) > 0 {
						pid = pids[0]
					}
				}
				pids = append(pids, pid)
			}
		} else if s == "attendance" {
			msg, ok := attend("api")
			fmt.Printf(
				`api attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
		}
	}
	return
}

func NewServer(address string, port int) *Server {
	s := &Server{
		Server: &http.Server{
			Addr:         net.JoinHostPort(address, strconv.Itoa(port)),
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		},
	}
	http.HandleFunc("/action", s.Action)
	return s
}

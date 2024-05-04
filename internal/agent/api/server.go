package api

import (
	"net"
	"net/http"
	"strconv"
)

type Server struct {
	*http.Server
	ProcessPids func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error)
}

func NewServer(host string, port int) *Server {
	mux := http.NewServeMux()

	s := &Server{
		Server: &http.Server{
			Handler: mux,
			Addr:    net.JoinHostPort(host, strconv.Itoa(port)),
		},
		ProcessPids: ProcessPidsWithMutex,
	}

	mux.HandleFunc("/action", s.Action)

	return s
}

func (s *Server) Serve() error {
	ln, err := net.Listen("tcp", s.Server.Addr)

	if err != nil {
		return err
	}

	return s.Server.Serve(ln)
}

func (s *Server) Addr() string {
	return s.Server.Addr
}

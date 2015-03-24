package main

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/logaggregator/snapshot"
	"github.com/flynn/flynn/pkg/connutil"
	"github.com/flynn/flynn/pkg/stream"
	"github.com/flynn/flynn/pkg/syslog/rfc5424"
	"github.com/flynn/flynn/pkg/syslog/rfc6587"

	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
)

type Server struct {
	*Aggregator
	*Replicator

	ll, rl, al net.Listener   // syslog, replication, and api listeners
	lwg, rwg   sync.WaitGroup // syslog & replication wait groups

	discd  *discoverd.Client
	hb     discoverd.Heartbeater
	srv    discoverd.Service
	stream stream.Stream
	eventc <-chan *discoverd.Event

	api      http.Handler
	shutdown chan struct{}
}

type ServerConfig struct {
	syslogAddr, replicationAddr, apiAddr string

	serviceName string
	discoverd   *discoverd.Client
}

func NewServer(conf ServerConfig) (*Server, error) {
	ll, err := net.Listen("tcp", conf.syslogAddr)
	if err != nil {
		return nil, err
	}

	rl, err := net.Listen("tcp", conf.replicationAddr)
	if err != nil {
		return nil, err
	}

	al, err := net.Listen("tcp", conf.apiAddr)
	if err != nil {
		return nil, err
	}

	eventc := make(chan *discoverd.Event)
	srv := conf.discoverd.Service(conf.serviceName)
	stream, err := srv.Watch(eventc)
	if err != nil {
		return nil, err
	}

	hb, err := conf.discoverd.AddServiceAndRegister(conf.serviceName, ll.Addr().String())
	if err != nil {
		return nil, err
	}

	a := NewAggregator()

	return &Server{
		Aggregator: a,
		Replicator: NewReplicator(),

		ll: ll,
		rl: rl,
		al: al,

		discd:  conf.discoverd,
		hb:     hb,
		srv:    srv,
		stream: stream,
		eventc: eventc,

		api:      apiHandler(a),
		shutdown: make(chan struct{}),
	}, nil
}

func (s *Server) Shutdown() {
	if err := s.stream.Close(); err != nil {
		log15.Error("event stream shutdown error", "err", err)
	}

	// close discoverd service heartbeater
	if err := s.hb.Close(); err != nil {
		log15.Error("heartbeat shutdown error", "err", err)
	}

	// shutdown listeners
	if err := s.ll.Close(); err != nil {
		log15.Error("syslog listener shutdown error", "err", err)
	}
	if err := s.rl.Close(); err != nil {
		log15.Error("replication listener shutdown error", "err", err)
	}
	if err := s.al.Close(); err != nil {
		log15.Error("api listener shutdown error", "err", err)
	}

	// close syslog & replication client connections
	close(s.shutdown)
	s.lwg.Wait()

	// shutdown aggregator & replicator
	s.Aggregator.Shutdown()
	s.Replicator.Shutdown()
	s.rwg.Wait()
}

func (s *Server) LoadSnapshot(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	sc := snapshot.NewScanner(f)
	for sc.Scan() {
		s.Aggregator.Feed(sc.Message)
	}
	return sc.Err()
}

func (s *Server) WriteSnapshot(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	buffers := s.Aggregator.CopyBuffers()
	return snapshot.WriteTo(buffers, f)
}

func (s *Server) SyslogAddr() net.Addr {
	return s.ll.Addr()
}

func (s *Server) Run() error {
	go s.runSyslog()
	go s.runReplication()
	go s.monitorDiscoverd()

	return http.Serve(s.al, s.api)
}

func (s *Server) runSyslog() {
	for {
		conn, err := s.ll.Accept()
		if err != nil {
			return
		}

		s.lwg.Add(1)
		go func(c net.Conn) {
			defer s.lwg.Done()
			s.drainSyslogConn(c)
		}(conn)
	}
}

func (s *Server) drainSyslogConn(conn net.Conn) {
	defer conn.Close()

	connDone := make(chan struct{})
	defer close(connDone)

	go func() {
		select {
		case <-connDone:
		case <-s.shutdown:
			conn.Close()
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Split(rfc6587.Split)
	for sc.Scan() {
		msgBytes := sc.Bytes()
		// slice in msgBytes could get modified on next Scan(), need to copy it
		msgCopy := make([]byte, len(msgBytes))
		copy(msgCopy, msgBytes)

		msg, err := rfc5424.Parse(msgCopy)
		if err != nil {
			log15.Error("rfc5424 parse error", "err", err)
		} else {
			s.Aggregator.Feed(msg)
			s.Replicator.Feed(msg)
		}
	}
}

func (s *Server) runReplication() {
	for {
		conn, err := s.rl.Accept()
		if err != nil {
			return
		}

		s.rwg.Add(1)
		go func(c net.Conn) {
			defer s.rwg.Done()
			s.fillReplicationConn(c)
		}(conn)
	}
}

func (s *Server) fillReplicationConn(conn net.Conn) {
	conn = connutil.CloseNotifyConn(conn)
	defer conn.Close()

	// pause the aggregator, shallow copy the aggregator's buffers, register a
	// replication stream, then unpause the aggregator
	unpause := s.Aggregator.Pause()
	buffers := s.Aggregator.CopyBuffers()
	msgc := s.Replicator.Register(conn.(connutil.CloseNotifier).CloseNotify())
	unpause()

	if err := snapshot.StreamTo(buffers, msgc, conn); err != nil {
		log15.Error("replication error", "err", err)
		go func() {
			for range msgc {
			}
		}()
	}
}

func (s *Server) monitorDiscoverd() {
	var unfollowc chan struct{}

	leader, err := s.srv.Leader()
	if err != nil {
		log15.Error("discoverd monitor error", "err", err)
	}
	if leader != nil {
		if leader.Addr == s.hb.Addr() {
			log15.Info("replication event", "status", "leader")
			return
		}
		if unfollowc, err = s.follow(leader.Addr); err != nil {
			log15.Error("replication error", "err", err)
		}
	}

	for event := range s.eventc {
		switch event.Kind {
		case discoverd.EventKindLeader:
			if unfollowc != nil {
				close(unfollowc)
			}

			leader = event.Instance
			if leader.Addr != s.hb.Addr() {
				if unfollowc, err = s.follow(leader.Addr); err != nil {
					log15.Error("replication error", "err", err)
				} else {
					log15.Info("replication event", "status", "follower", "leader", leader.Addr)
				}
			} else {
				log15.Info("replication event", "status", "leader")
				return
			}
		}
	}
}

func (s *Server) follow(addr string) (chan struct{}, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	s.Aggregator.Flush()

	unfollowc := make(chan struct{})
	go func() {
		defer conn.Close()
		sc := snapshot.NewScanner(conn)

		for sc.Scan() {
			select {
			case <-unfollowc:
				return
			default:
			}

			s.Aggregator.Feed(sc.Message)
		}
	}()

	return unfollowc, nil
}

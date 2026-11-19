package goredis_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/canton"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/goredis"
	"github.com/FTP-Tech-LLC/x402-canton-merchant/go/internal/testkit"
)

const slotCount = 16384

// slotOf is the Redis Cluster key slot: CRC16 (XMODEM) of the hash tag, or of the key.
func slotOf(key string) int {
	if start := strings.IndexByte(key, '{'); start >= 0 {
		if end := strings.IndexByte(key[start+1:], '}'); end > 0 {
			key = key[start+1 : start+1+end]
		}
	}
	crc := 0
	for _, b := range []byte(key) {
		crc ^= int(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
			crc &= 0xFFFF
		}
	}
	return crc % slotCount
}

// clusterNode is a scripted RESP endpoint that owns a slot range and answers MOVED
// for the rest, as a node of a Redis Cluster does.
type clusterNode struct {
	listener net.Listener
	low      int // owned slots: low to high inclusive
	high     int
	peer     *clusterNode
}

func startCluster(t *testing.T) (a, b *clusterNode) {
	t.Helper()
	listen := func() net.Listener {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	a = &clusterNode{listener: listen(), low: 0, high: slotCount/2 - 1}
	b = &clusterNode{listener: listen(), low: slotCount / 2, high: slotCount - 1}
	a.peer, b.peer = b, a
	go a.serve()
	go b.serve()
	return a, b
}

func (n *clusterNode) addr() string { return n.listener.Addr().String() }

func (n *clusterNode) serve() {
	for {
		conn, err := n.listener.Accept()
		if err != nil {
			return
		}
		go n.handle(conn)
	}
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for range count {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(header[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		read := 0
		for read < len(buf) {
			m, err := reader.Read(buf[read:])
			if err != nil {
				return nil, err
			}
			read += m
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func (n *clusterNode) slotsReply() string {
	entry := func(node *clusterNode) string {
		host, port, _ := net.SplitHostPort(node.addr())
		portNumber, _ := strconv.Atoi(port)
		return fmt.Sprintf("*3\r\n:%d\r\n:%d\r\n*3\r\n$%d\r\n%s\r\n:%d\r\n$4\r\nnode\r\n", node.low, node.high, len(host), host, portNumber)
	}
	return "*2\r\n" + entry(n) + entry(n.peer)
}

func (n *clusterNode) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		command := strings.ToUpper(args[0])
		var reply string
		switch {
		case command == "HELLO":
			reply = "-ERR unknown command 'HELLO'\r\n"
		case command == "CLUSTER" && len(args) > 1 && strings.EqualFold(args[1], "SLOTS"):
			reply = n.slotsReply()
		case command == "GET" || command == "SET" || command == "DEL" || command == "EVAL" || command == "EVALSHA":
			key := args[1]
			if command == "EVAL" || command == "EVALSHA" {
				key = args[3]
			}
			if slot := slotOf(key); slot < n.low || slot > n.high {
				reply = fmt.Sprintf("-MOVED %d %s\r\n", slot, n.peer.addr())
			} else if command == "GET" {
				reply = "$-1\r\n"
			} else if command == "SET" {
				reply = "+OK\r\n"
			} else {
				reply = ":0\r\n"
			}
		default:
			reply = "+OK\r\n"
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}

func newGateWith(t *testing.T, store canton.Store) error {
	t.Helper()
	f := testkit.StartFacilitator(t)
	gate, err := canton.NewGate(context.Background(), canton.Config{FacilitatorURL: f.URL, Routes: testkit.Routes(),
		SupportedWait: 2 * time.Second, Logger: slog.New(slog.DiscardHandler), Store: store})
	if err == nil {
		t.Cleanup(func() { _ = gate.Close(context.Background()) })
	}
	return err
}

// A plain client on a cluster endpoint gets MOVED for keys in other slots: the lock,
// record and claim of one payment would land on different nodes. Startup must
// refuse it whatever slot one random probe key would have had.
func TestPlainClientOnAClusterEndpointFailsStartup(t *testing.T) {
	a, _ := startCluster(t)
	plain := redis.NewClient(&redis.Options{Addr: a.addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = plain.Close() })
	// Each startup probes a random key: a check that depends on its slot passes about
	// half of them. Eight in a row are refused only by a check across all slots.
	for range 8 {
		started := time.Now()
		err := newGateWith(t, goredis.NewStore(plain, "x402:test:"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "payment store")
		assert.Contains(t, err.Error(), "MOVED")
		assert.Contains(t, err.Error(), "redis.NewClusterClient")
		assert.Less(t, time.Since(started), 1500*time.Millisecond, "a wiring mistake is not retried for SupportedWait")
	}
}

func TestClusterClientOnAClusterEndpointStarts(t *testing.T) {
	a, _ := startCluster(t)
	cluster := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{a.addr()}, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = cluster.Close() })
	require.NoError(t, newGateWith(t, goredis.NewStore(cluster, "x402:test:")))
}

func TestSingleRedisStarts(t *testing.T) {
	store, _ := newStore(t)
	require.NoError(t, newGateWith(t, store))
}

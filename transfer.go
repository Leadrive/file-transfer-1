package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const SERVER_PORT = ":55555"

type Speed struct {
	total int
	bytes []int
	done  chan struct{}
	wait  chan struct{}
}

func NewSpeed() *Speed {
	s := &Speed{
		bytes: make([]int, 10),
		done:  make(chan struct{}),
		wait:  make(chan struct{}),
	}
	go s.tick()
	return s
}

func (s *Speed) Write(p []byte) (int, error) {
	s.total += len(p)
	s.bytes[time.Now().Unix()%int64(len(s.bytes))] += len(p)
	return len(p), nil
}

func (s *Speed) Close() error {
	close(s.done)
	<-s.wait
	return nil
}

func (s *Speed) tick() {
	start := time.Now()

LOOP:
	for {
		select {
		case now := <-time.After(time.Second):
			n := int(now.Unix()-1) % len(s.bytes)
			s.Print(n)
			s.bytes[n] = 0
		case <-s.done:
			break LOOP
		}
	}

	spent := time.Now().Sub(start)
	fmt.Printf("Total send: %v, time usage: %v\n", Size(s.total), spent)
	close(s.wait)
}

func Size(b int) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.3f KB", float64(b)/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.3f MB", float64(b)/1024/1024)
	default:
		return fmt.Sprintf("%.3f GB", float64(b)/1024/1024/1024)
	}
}

func (s *Speed) Print(n int) {
	out := fmt.Sprintf("recv: %v/s, total: %v      ",
		Size(s.bytes[n]), Size(s.total))

	fmt.Print(out)
	for i := 0; i < len(out); i++ {
		fmt.Print("\b")
	}
}

// - - - - - - - - - - server - - - - - - - - - -

func handle(conn net.Conn) {
	defer conn.Close()

	b := make([]byte, 256)
	n, err := conn.Read(b)
	if err != nil {
		if err != io.EOF { // 探测连接
			fmt.Println("recv file name error:", err)
		}
		return
	}

	file := string(b[:n])
	fp, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("create file %q error: %v\n", file, err)
		return
	}
	defer fp.Close()
	fi, err := fp.Stat()
	if err != nil {
		fmt.Printf("get file %q stat error: %v", file, err)
		return
	}
	defer fp.Sync()

	binary.Write(conn, binary.BigEndian, fi.Size())
	fmt.Printf("ready to recv %q from %v\n", file, fi.Size())

	speed := NewSpeed()
	nc, err := io.Copy(io.MultiWriter(fp, speed), conn)
	speed.Close()
	if err != nil {
		if err != io.EOF {
			fmt.Printf("copy file %q error: %v\n", file, err)
			return
		}
	}

	fmt.Printf("Save file %q OK! Total recv: %v\n", file, Size(int(nc)))
}

func serve() {
	listener, err := net.Listen("tcp", SERVER_PORT)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Printf("serve start listen %v\n", SERVER_PORT)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("accept error:", err)
			continue
		}
		// 不允许并发写，不然输出会乱，而且已经充分利用起带宽
		handle(conn)
	}
}

// - - - - - - - - - - client - - - - - - - - - -

func getInnerIps() []string {
	info, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}

	var ips []string
	for _, addr := range info {
		ipMask := strings.Split(addr.String(), "/")

		ip := ipMask[0]
		// 排除ipv6
		if !strings.Contains(ip, ".") {
			continue
		}

		// 排除lo
		if ip == "127.0.0.1" {
			continue
		}

		// 排除公网
		if strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") || strings.HasPrefix(ip, "172.") {
			ips = append(ips, ip)
		}
	}
	return ips
}

func getIpPrefix(s string) string {
	if s != "" {
		idx := strings.LastIndex(s, ".")
		if idx > 0 {
			s = s[:idx+1]
		}
	}
	return s
}

func ScanServerHosts() []string {
	ips := getInnerIps()
	if len(ips) == 0 {
		return nil
	}

	var hosts []string
	var mut sync.Mutex
	for _, ip := range ips {
		var wg sync.WaitGroup
		prefix := getIpPrefix(ip)
		for i := 1; i <= 254; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				host := fmt.Sprintf("%v%v%v", prefix, i, SERVER_PORT)
				conn, err := (&net.Dialer{
					Timeout: 2 * time.Second, // 局域网，2秒足矣
				}).Dial("tcp", host)
				if err == nil {
					conn.Close()
					mut.Lock()
					hosts = append(hosts, host)
					mut.Unlock()
				}
			}(i)
		}
		wg.Wait()
	}
	return hosts
}

func IsClosed(conn net.Conn) bool {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return true
	}
	fd, err := tcp.File()
	if err != nil {
		return true
	}
	if fd == nil {
		return true
	}
	fd.Close()
	return false
}

func UploadTo(host string, file string) {
	conn, err := net.Dial("tcp", host)
	if err != nil {
		fmt.Printf("upload to %q file %q failed when dial error: %v\n", host, file, err)
		return
	}
	defer conn.Close()

	n, err := conn.Write([]byte(file))
	if err != nil || n != len(file) {
		fmt.Printf("send to %q file name %q error: %v\n", host, file, err)
		return
	}

	var size int64
	err = binary.Read(conn, binary.BigEndian, &size)
	if err != nil {
		if IsClosed(conn) || err == io.EOF {
			fmt.Printf("Server %q already has %q\n", host, file)
			return
		}
		fmt.Printf("recv server %q ack file %q error: %v\n", host, file, err)
		return
	}

	fp, err := os.Open(file)
	if err != nil {
		fmt.Printf("open file %q error: %v\n", file, err)
		return
	}
	defer fp.Close()

	// 续传
	c, err := fp.Seek(size, io.SeekStart)
	if err != nil {
		fmt.Printf("seek file to %v error: %v\n", size, err)
		return
	}
	fmt.Printf("ready to send %q to %q, continue at %v\n", file, host, c)

	speed := NewSpeed()
	nc, err := io.Copy(io.MultiWriter(conn, speed), fp)
	speed.Close()
	if err != nil {
		if err != io.EOF {
			fmt.Printf("send to %q file %q error: %v", host, file, err)
			return
		}
	}

	fmt.Printf("Save file %q to server %q OK! Total send: %v\n", file, host, Size(int(nc)))
}

func client() {
	ls, err := ioutil.ReadDir(".")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Println("Finding servers...")
	hosts := ScanServerHosts()
	if len(hosts) == 0 {
		fmt.Println("No server found")
		os.Exit(1)
	}
	fmt.Printf("Server hosts: %q\n", hosts)

	for _, f := range ls {
		if f.IsDir() {
			continue
		}
		for _, host := range hosts {
			UploadTo(host, f.Name())
		}
	}
}

// - - - - - - - - - - main - - - - - - - - - -

func isClient() bool {
	if len(os.Args) < 2 {
		return false
	}
	arg := strings.TrimPrefix(os.Args[1], "-")
	if arg == "" {
		return false
	}
	return arg[0] == 'c'
}

func main() {
	if isClient() {
		client()
	} else {
		serve()
	}
}

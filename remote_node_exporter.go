// License:
//     MIT License, Copyright phuslu@hotmail.com
// Usage:
//     env PORT=9101 SSH_HOST=192.168.2.1 SSH_USER=admin SSH_PASS=123456 ./remote_node_exporter

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ErikDubbelboer/gspt"
	"golang.org/x/crypto/ssh"
)

var (
	Port            = os.Getenv("PORT")
	SshHost         = os.Getenv("SSH_HOST")
	SshPort         = os.Getenv("SSH_PORT")
	SshUser         = os.Getenv("SSH_USER")
	SshPass         = os.Getenv("SSH_PASS")
	ExtraCollectors = os.Getenv("EXTRA_COLLECTORS")
	TextfilePath    = os.Getenv("TEXTFILE_PATH")
)

var PreReadFileList []string = []string{
	"/etc/storage/system_time",
	"/proc/diskstats",
	"/proc/driver/rtc",
	"/proc/loadavg",
	"/proc/meminfo",
	"/proc/mounts",
	"/proc/net/arp",
	"/proc/net/dev",
	"/proc/net/netstat",
	"/proc/net/snmp",
	"/proc/net/sockstat",
	"/proc/stat",
	"/proc/sys/fs/file-nr",
	"/proc/sys/kernel/random/entropy_avail",
	"/proc/sys/net/netfilter/nf_conntrack_count",
	"/proc/sys/net/netfilter/nf_conntrack_max",
	"/proc/vmstat",
}

type Client struct {
	Addr   string
	Config *ssh.ClientConfig

	client     *ssh.Client
	timeOffset time.Duration
	once       sync.Once
}

func (c *Client) Execute(cmd string) (string, error) {
	c.once.Do(func() {
		if c.client == nil {
			var err error
			c.client, err = ssh.Dial("tcp", c.Addr, c.Config)
			if err != nil {
				log.Printf("ssh.Dial(%#v) error: %+v", c.Addr, err)
			}
		}
	})

	session, err := c.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b

	err = session.Run(cmd)

	return b.String(), err
}

type ProcFile struct {
	Text string
	Sep  string
}

func (pf ProcFile) sep() string {
	if pf.Sep != "" {
		return pf.Sep
	}
	return " "
}

func (pf ProcFile) Int() (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(pf.Text), 10, 64)
}

func (pf ProcFile) Float() (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(pf.Text), 64)
}

func (pf ProcFile) KV() map[string]string {
	m := make(map[string]string)

	scanner := bufio.NewScanner(strings.NewReader(pf.Text))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), pf.sep(), 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		m[key] = value
	}

	return m
}

type Metrics struct {
	Client *Client

	name    string
	body    bytes.Buffer
	preread map[string]string
}

func (m *Metrics) PreRead() error {
	m.preread = make(map[string]string)

	cmd := "/bin/fgrep \"\" " + strings.Join(PreReadFileList, " ")
	if TextfilePath != "" {
		cmd += " " + TextfilePath
	}

	output, _ := m.Client.Execute(cmd)

	split := func(s string) map[string]string {
		m := make(map[string]string)
		var lastname string
		var b bytes.Buffer

		scanner := bufio.NewScanner(strings.NewReader(s))
		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), ":", 2)
			if len(parts) != 2 {
				continue
			}

			filename := strings.TrimSpace(parts[0])
			line := parts[1]

			if filename != lastname {
				if lastname != "" {
					m[lastname] = b.String()
				}
				b.Reset()
				lastname = filename
			}

			b.WriteString(line)
			b.WriteString("\n")
		}
		m[lastname] = b.String()
		return m
	}

	m.preread = split(output)

	for _, filename := range PreReadFileList {
		if _, ok := m.preread[filename]; !ok {
			m.preread[filename] = ""
		}
	}

	return nil
}

func (m *Metrics) ReadFile(filename string) (string, error) {
	s, ok := m.preread[filename]
	if ok {
		return s, nil
	}

	return m.Client.Execute("/bin/cat " + filename)
}

func (m *Metrics) PrintType(name string, typ string, help string) {
	m.name = name
	if help != "" {
		m.body.WriteString(fmt.Sprintf("# HELP %s %s.\n", name, help))
	}
	m.body.WriteString(fmt.Sprintf("# TYPE %s %s\n", name, typ))
}

func (m *Metrics) PrintFloat(labels string, value float64) {
	if labels != "" {
		m.body.WriteString(fmt.Sprintf("%s{%s} ", m.name, labels))
	} else {
		m.body.WriteString(fmt.Sprintf("%s ", m.name))
	}

	if value >= 1000000 {
		m.body.WriteString(fmt.Sprintf("%e\n", value))
	} else {
		m.body.WriteString(fmt.Sprintf("%f\n", value))
	}
}

func (m *Metrics) PrintInt(labels string, value int64) {
	if labels != "" {
		m.body.WriteString(fmt.Sprintf("%s{%s} ", m.name, labels))
	} else {
		m.body.WriteString(fmt.Sprintf("%s ", m.name))
	}

	if value >= 1000000 {
		m.body.WriteString(fmt.Sprintf("%e\n", float64(value)))
	} else {
		m.body.WriteString(fmt.Sprintf("%d\n", value))
	}
}

func (m *Metrics) CollectTime() error {
	var nsec int64
	var t time.Time

	s, err := m.ReadFile("/proc/driver/rtc")

	if s != "" {
		kv := (ProcFile{Text: s, Sep: ":"}).KV()
		date := kv["rtc_date"] + " " + kv["rtc_time"]
		t, err = time.Parse("2006-01-02 15:04:05", date)
		nsec = t.Unix()
	}

	if nsec == 0 {
		s, err = m.ReadFile("/etc/storage/system_time")
		nsec, err = (ProcFile{Text: s}).Int()
	}

	if nsec == 0 {
		s, err = m.Client.Execute("date +%s")
		nsec, err = (ProcFile{Text: s}).Int()
	}

	if nsec != 0 {
		m.PrintType("node_time", "counter", "System time in seconds since epoch (1970)")
		m.PrintInt("", nsec)
	}

	return err
}

func (m *Metrics) CollectLoadavg() error {
	s, err := m.ReadFile("/proc/loadavg")
	if err != nil {
		return err
	}

	parts := strings.Split(strings.TrimSpace(s), " ")
	if len(parts) < 3 {
		return fmt.Errorf("Unknown loadavg %#v", s)
	}

	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return err
	}

	m.PrintType("node_load1", "gauge", "1m load average")
	m.PrintFloat("", v)

	return nil
}

func (m *Metrics) CollectAll() (string, error) {
	var err error

	err = m.PreRead()
	if err != nil {
		log.Printf("%T.PreRead() error: %+v", m, err)
	}

	m.CollectTime()
	m.CollectLoadavg()

	return m.body.String(), nil
}

func main() {
	if SshPort == "" {
		SshPort = "22"
	}

	client := &Client{
		Addr: net.JoinHostPort(SshHost, SshPort),
		Config: &ssh.ClientConfig{
			User: SshUser,
			Auth: []ssh.AuthMethod{
				ssh.Password(SshPass),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
	}

	if runtime.GOOS == "linux" {
		gspt.SetProcTitle(fmt.Sprintf("remote_node_exporter: [%s] listening %s", client.Addr, Port))
	}

	http.HandleFunc("/metrics", func(rw http.ResponseWriter, req *http.Request) {
		m := Metrics{
			Client: client,
		}

		s, err := m.CollectAll()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusServiceUnavailable)
			return
		}

		io.WriteString(rw, s)
	})

	log.Fatal(http.ListenAndServe(":"+Port, nil))
}

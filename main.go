// sharewifi provides a small web console for a Linux hostapd hotspot.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

//go:embed web.html
var webFS embed.FS

// Version is intentionally embedded in every binary so deployed instances can
// be identified without starting the web service.
const Version = "v0.1.7"

// infoEnabled is immutable after flag parsing and controls diagnostic command tracing.
var infoEnabled bool

type Config struct {
	// Service settings are deliberately not part of the web/config-file model.
	// They are supplied at process start and remain in server memory only.
	Listen            string `json:"-"`
	Workdir           string `json:"-"`
	ConsoleUsername   string `json:"-"`
	ConsolePassword   string `json:"-"`
	Interface         string `json:"interface"`
	SSID              string `json:"ssid"`
	Passphrase        string `json:"passphrase"`
	CountryCode       string `json:"country_code"`
	Band              string `json:"band"`
	Channel           int    `json:"channel"`
	GatewayCIDR       string `json:"gateway_cidr"`
	DHCPStart         string `json:"dhcp_start"`
	DHCPEnd           string `json:"dhcp_end"`
	LeaseTime         string `json:"lease_time"`
	UpstreamInterface string `json:"upstream_interface"`
	AllowUpstreamLAN  bool   `json:"allow_upstream_lan"`
	UpstreamLANCIDR   string `json:"upstream_lan_cidr"`
}

type Check struct {
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Required bool   `json:"required"`
	Help     string `json:"help"`
}
type Status struct {
	Running  bool    `json:"running"`
	Message  string  `json:"message"`
	Config   *Config `json:"config,omitempty"`
	Workdir  string  `json:"-"`
	Checks   []Check `json:"checks"`
	Firewall string  `json:"firewall"`
	Logs     string  `json:"logs,omitempty"`
}
type ClientStatus struct {
	Clients     []ClientTraffic  `json:"clients"`
	Interface   InterfaceTraffic `json:"interface"`
	DHCPBackend string           `json:"dhcp_backend,omitempty"`
	Error       string           `json:"error,omitempty"`
}
type ClientTraffic struct {
	MAC     string  `json:"mac"`
	IP      string  `json:"ip,omitempty"`
	Name    string  `json:"name,omitempty"`
	Signal  string  `json:"signal,omitempty"`
	TXRate  string  `json:"tx_rate,omitempty"`
	RXRate  string  `json:"rx_rate,omitempty"`
	TXBytes uint64  `json:"tx_bytes"`
	RXBytes uint64  `json:"rx_bytes"`
	TXBPS   float64 `json:"tx_bps"`
	RXBPS   float64 `json:"rx_bps"`
}
type InterfaceTraffic struct {
	RXBytes uint64  `json:"rx_bytes"`
	TXBytes uint64  `json:"tx_bytes"`
	RXBPS   float64 `json:"rx_bps"`
	TXBPS   float64 `json:"tx_bps"`
}
type WirelessCapabilities struct {
	Channels24GHz []int  `json:"channels_24ghz"`
	Channels5GHz  []int  `json:"channels_5ghz"`
	Supports5GHz  bool   `json:"supports_5ghz"`
	AutoChannel   bool   `json:"auto_channel"`
	Source        string `json:"source,omitempty"`
	Error         string `json:"error,omitempty"`
	auto24GHz     []int
	auto5GHz      []int
}
type trafficSample struct {
	rx, tx uint64
	at     time.Time
}
type app struct {
	mu              sync.Mutex
	workdir         string
	checks          []Check
	fw              string
	running         bool
	cfg             *Config
	hostapd, dhcp   *exec.Cmd
	dhcpBackend     string
	nmManaged       bool
	nmInterface     string
	oldForward      string
	lastError       string
	stopping        bool
	baseConfig      Config
	clientSamples   map[string]trafficSample
	interfaceSample trafficSample
}

func main() {
	var listen, workdir, configPath, username, password string
	var startupDelay int
	var showVersion bool
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "ShareWiFi %s\n\nUsage: sharewifi [options]\n\nOptions:\n", Version)
		flag.PrintDefaults()
	}
	flag.StringVar(&listen, "listen", "0.0.0.0:8080", "web listen address")
	flag.StringVar(&workdir, "workdir", "", "runtime directory")
	flag.StringVar(&username, "username", "", "console basic-auth username")
	flag.StringVar(&password, "password", "", "console basic-auth password")
	flag.StringVar(&configPath, "config", "", "JSON configuration to start")
	flag.IntVar(&startupDelay, "delay", 0, "seconds to delay startup from --config")
	flag.BoolVar(&infoEnabled, "info", false, "print executed system commands and their purpose")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Println(Version)
		return
	}
	if startupDelay < 0 {
		log.Fatal("--delay must be zero or a positive number of seconds")
	}
	if configPath != "" {
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "config", "listen", "workdir", "username", "password", "delay", "info":
				// These process-level settings are intentionally not stored in JSON.
			default:
				log.Fatalf("--config cannot be combined with --%s", f.Name)
			}
		})
	}
	if (username == "") != (password == "") {
		log.Fatal("--username and --password must be supplied together")
	}
	if runtime.GOOS != "linux" {
		log.Fatal("sharewifi only supports Linux")
	}
	if os.Geteuid() != 0 {
		log.Fatal("root is required: sudo sharewifi")
	}
	if workdir == "" {
		var err error
		workdir, err = os.MkdirTemp("", "sharewifi-")
		if err != nil {
			log.Fatal(err)
		}
	} else if err := os.MkdirAll(workdir, 0700); err != nil {
		log.Fatal(err)
	}
	// Keep the server-side defaults in sync with the embedded form.  The status
	// endpoint is also the source of truth used to populate the form, so it
	// must never publish an otherwise-valid configuration with empty defaults.
	a := &app{workdir: workdir, clientSamples: make(map[string]trafficSample), baseConfig: Config{
		Listen:          listen,
		Workdir:         workdir,
		ConsoleUsername: username,
		ConsolePassword: password,
		SSID:            "ShareWiFi",
		Passphrase:      "change-me-123",
		CountryCode:     "CN",
		Band:            "2.4GHz",
		Channel:         6,
		GatewayCIDR:     "192.168.50.1/24",
		DHCPStart:       "192.168.50.20",
		DHCPEnd:         "192.168.50.200",
		LeaseTime:       "12h",
	}}
	a.checks, a.fw, a.dhcpBackend = environment()
	var delayedConfig *Config
	if configPath != "" {
		cfg, err := readConfig(configPath)
		if err != nil {
			log.Fatal(err)
		}
		// Keep the web form aligned with a configuration that is waiting for a
		// delayed start, rather than showing the built-in defaults meanwhile.
		a.baseConfig = cfg
		if startupDelay == 0 {
			if err = a.start(cfg); err != nil {
				log.Fatal(err)
			}
		} else {
			delayedConfig = &cfg
		}
	}
	mux := http.NewServeMux()
	a.routes(mux)
	handler := basicAuth(mux, username, password)
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if username == "" {
			log.Printf("WARNING: console has no authentication; listening on http://%s", listen)
		} else {
			log.Printf("console basic authentication enabled; listening on http://%s", listen)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web server: %v", err)
		}
	}()
	var cancelDelayedStart chan struct{}
	var delayedStartDone chan struct{}
	if delayedConfig != nil {
		cancelDelayedStart = make(chan struct{})
		delayedStartDone = make(chan struct{})
		log.Printf("configuration loaded; hotspot startup will begin in %d seconds", startupDelay)
		go func(cfg Config, delay int) {
			defer close(delayedStartDone)
			timer := time.NewTimer(time.Duration(delay) * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				log.Printf("delayed hotspot startup begins")
				if err := a.start(cfg); err != nil {
					log.Printf("delayed hotspot startup failed: %v", err)
				}
			case <-cancelDelayedStart:
				log.Printf("delayed hotspot startup cancelled")
			}
		}(*delayedConfig, startupDelay)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	if cancelDelayedStart != nil {
		close(cancelDelayedStart)
		<-delayedStartDone
	}
	a.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func basicAuth(next http.Handler, username, password string) http.Handler {
	if username == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		validUser := subtle.ConstantTimeCompare([]byte(u), []byte(username)) == 1
		validPassword := subtle.ConstantTimeCompare([]byte(p), []byte(password)) == 1
		if !ok || !validUser || !validPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="ShareWiFi"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) routes(m *http.ServeMux) {
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := webFS.ReadFile("web.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	m.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		jsonOut(w, Status{Running: a.running, Message: statusMessage(a.running, a.lastError), Config: a.exportConfig(), Workdir: a.workdir, Checks: a.checks, Firewall: a.fw, Logs: a.logs()})
	})
	m.HandleFunc("/api/interfaces", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, wirelessInterfaces()) })
	m.HandleFunc("/api/capabilities", func(w http.ResponseWriter, r *http.Request) {
		iface := r.URL.Query().Get("interface")
		if iface == "" {
			jsonErr(w, http.StatusBadRequest, errors.New("interface is required"))
			return
		}
		jsonOut(w, wirelessCapabilities(iface))
	})
	m.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, a.clients()) })
	m.HandleFunc("/api/config", a.configAPI)
	m.HandleFunc("/api/start", a.startAPI)
	m.HandleFunc("/api/stop", a.stopAPI)
}
func statusMessage(r bool, lastError string) string {
	if r {
		return "Wi-Fi sharing is running"
	}
	if lastError != "" {
		return "Wi-Fi sharing failed: " + lastError
	}
	return "Wi-Fi sharing is stopped"
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func jsonErr(w http.ResponseWriter, code int, e error) {
	w.WriteHeader(code)
	jsonOut(w, map[string]string{"error": e.Error()})
}

func (a *app) configAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		jsonOut(w, a.exportConfig())
		return
	}
	cfg, err := decodeConfig(r.Body)
	if err != nil {
		jsonErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=sharewifi.json")
	jsonOut(w, cfg)
}
func (a *app) startAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, errors.New("POST required"))
		return
	}
	cfg, err := decodeConfig(r.Body)
	if err == nil {
		err = a.start(cfg)
	}
	if err != nil {
		log.Printf("hotspot start request rejected: %v", err)
		jsonErr(w, 400, err)
		return
	}
	jsonOut(w, map[string]string{"message": "started"})
}
func (a *app) stopAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, errors.New("POST required"))
		return
	}
	a.stop()
	jsonOut(w, map[string]string{"message": "stopped"})
}

func environment() ([]Check, string, string) {
	cmds := []struct {
		name     string
		required bool
	}{{"hostapd", true}, {"ip", true}, {"iw", true}, {"hostapd_cli", false}}
	checks := make([]Check, 0, len(cmds)+2)
	for _, c := range cmds {
		_, e := exec.LookPath(c.name)
		checks = append(checks, Check{c.name, e == nil, c.required, installHelp(c.name)})
	}
	_, dnsmasq := exec.LookPath("dnsmasq")
	_, udhcpd := exec.LookPath("udhcpd")
	dhcp := ""
	if dnsmasq == nil {
		dhcp = "dnsmasq"
	} else if udhcpd == nil {
		dhcp = "udhcpd"
	}
	dhcpHelp := "Debian/Ubuntu：sudo apt install dnsmasq 或 sudo apt install udhcpd；Fedora/CentOS：sudo dnf install dnsmasq 或 sudo dnf install udhcpd"
	if dhcp != "" {
		dhcpHelp = "当前使用 " + dhcp
	}
	checks = append(checks, Check{"dnsmasq 或 udhcpd", dhcp != "", true, dhcpHelp})
	_, nft := exec.LookPath("nft")
	_, ipt := exec.LookPath("iptables")
	fw := ""
	if nft == nil {
		fw = "nftables"
	} else if ipt == nil {
		fw = "iptables"
	}
	fwHelp := "Debian/Ubuntu：sudo apt install nftables 或 sudo apt install iptables；Fedora/CentOS：sudo dnf install nftables 或 sudo dnf install iptables"
	if fw != "" {
		if fw == "nftables" {
			fwHelp = "当前使用 nft（nftables）"
		} else {
			fwHelp = "当前使用 iptables"
		}
	}
	checks = append(checks, Check{"nft 或 iptables", fw != "", true, fwHelp})
	return checks, fw, dhcp
}
func installHelp(name string) string {
	pkg := name
	if name == "ip" {
		pkg = "iproute2"
	}
	return "Debian/Ubuntu: sudo apt install " + pkg + "; Fedora/CentOS: sudo dnf install " + pkg
}
func missingChecks(c []Check) error {
	var x []string
	for _, v := range c {
		if v.Required && !v.Present {
			x = append(x, v.Name+" ("+v.Help+")")
		}
	}
	if len(x) > 0 {
		return errors.New("missing required programs: " + strings.Join(x, "; "))
	}
	return nil
}

func decodeConfig(r io.Reader) (Config, error) {
	var c Config
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	return c, validate(c)
}
func readConfig(p string) (Config, error) {
	f, e := os.Open(p)
	if e != nil {
		return Config{}, e
	}
	defer f.Close()
	return decodeConfig(f)
}
func validate(c Config) error {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("listen must be an address such as 0.0.0.0:8080")
	}
	if (c.ConsoleUsername == "") != (c.ConsolePassword == "") {
		return errors.New("console username and password must both be set or both be empty")
	}
	if hasControl(c.ConsoleUsername) || hasControl(c.ConsolePassword) {
		return errors.New("console credentials cannot contain control characters")
	}
	if strings.TrimSpace(c.Interface) == "" || strings.ContainsAny(c.Interface, " /\t\n") {
		return errors.New("a valid wireless interface is required")
	}
	if len(c.SSID) < 1 || len(c.SSID) > 32 || hasControl(c.SSID) {
		return errors.New("SSID must be 1-32 characters")
	}
	if len(c.Passphrase) < 8 || len(c.Passphrase) > 63 || hasControl(c.Passphrase) {
		return errors.New("password must be 8-63 characters")
	}
	if len(c.CountryCode) != 2 || !asciiLetters(c.CountryCode) {
		return errors.New("country code must have two letters")
	}
	c.CountryCode = strings.ToUpper(c.CountryCode)
	if c.Band != "2.4GHz" && c.Band != "5GHz" {
		return errors.New("band must be 2.4GHz or 5GHz")
	}
	if c.Channel < 0 || c.Channel > 196 {
		return errors.New("invalid channel; use 0 for automatic selection")
	}
	ip, n, e := net.ParseCIDR(c.GatewayCIDR)
	ones, bits := 0, 0
	if n != nil {
		ones, bits = n.Mask.Size()
	}
	if e != nil || ip.To4() == nil || bits != 32 || ones < 1 || ones > 30 {
		return errors.New("gateway must be an IPv4 CIDR")
	}
	s := net.ParseIP(c.DHCPStart).To4()
	end := net.ParseIP(c.DHCPEnd).To4()
	if s == nil || end == nil || !n.Contains(s) || !n.Contains(end) || bytesCompare(s, end) > 0 {
		return errors.New("DHCP range must be ordered IPv4 addresses inside gateway subnet")
	}
	if _, e = time.ParseDuration(c.LeaseTime); e != nil {
		return errors.New("lease time must be a Go duration such as 12h")
	}
	if c.UpstreamInterface != "" && strings.ContainsAny(c.UpstreamInterface, " /\t\n") {
		return errors.New("invalid upstream interface")
	}
	if c.AllowUpstreamLAN {
		upstreamIP, upstreamNet, err := net.ParseCIDR(c.UpstreamLANCIDR)
		if err != nil || upstreamIP.To4() == nil || upstreamNet.IP.To4() == nil {
			return errors.New("upstream LAN must be an IPv4 CIDR such as 172.16.41.0/24")
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
func asciiLetters(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}
func bytesCompare(a, b net.IP) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func (a *app) start(c Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return errors.New("Wi-Fi sharing is already running")
	}
	a.lastError = ""
	a.clientSamples = make(map[string]trafficSample)
	a.interfaceSample = trafficSample{}
	if err := missingChecks(a.checks); err != nil {
		return err
	}
	if err := a.prepareLogs(); err != nil {
		return err
	}
	if !contains(wirelessInterfaces(), c.Interface) {
		return fmt.Errorf("%s is not an available wireless interface", c.Interface)
	}
	if !wirelessAPCapable(c.Interface) {
		return fmt.Errorf("%s does not report AP mode support", c.Interface)
	}
	upstream := c.UpstreamInterface
	if upstream == "" {
		var e error
		upstream, e = defaultInterface()
		if e != nil {
			return e
		}
		c.UpstreamInterface = upstream
	}
	if upstream == c.Interface {
		return errors.New("upstream interface cannot be the AP interface")
	}
	var channelErr error
	c.Channel, channelErr = resolveChannel(c.Interface, c.Band, c.Channel)
	if channelErr != nil {
		return channelErr
	}
	if err := a.unmanageNM(c.Interface); err != nil {
		return err
	}
	a.cfg = &c
	rollback := func(e error) error { a.lastError = e.Error(); a.cleanupLocked(); return e }
	if err := run("ip", "link", "set", c.Interface, "down"); err != nil {
		return rollback(err)
	}
	if err := run("ip", "addr", "flush", "dev", c.Interface); err != nil {
		return rollback(err)
	}
	if err := run("ip", "addr", "add", c.GatewayCIDR, "dev", c.Interface); err != nil {
		return rollback(err)
	}
	if err := run("ip", "link", "set", c.Interface, "up"); err != nil {
		return rollback(err)
	}
	a.oldForward = readTrim("/proc/sys/net/ipv4/ip_forward")
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
		return rollback(err)
	}
	if err := a.addFirewall(c); err != nil {
		return rollback(err)
	}
	if err := a.writeConfigs(c); err != nil {
		return rollback(err)
	}
	hostLog, err := os.OpenFile(filepath.Join(a.workdir, "hostapd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return rollback(err)
	}
	a.hostapd = exec.Command("hostapd", filepath.Join(a.workdir, "hostapd.conf"))
	a.hostapd.Stdout = hostLog
	a.hostapd.Stderr = hostLog
	logCommand(a.hostapd.Path, a.hostapd.Args[1:]...)
	if err := a.hostapd.Start(); err != nil {
		_ = hostLog.Close()
		return rollback(fmt.Errorf("hostapd: %w", err))
	}
	hostapdCmd := a.hostapd
	go func(cmd *exec.Cmd) {
		err := cmd.Wait()
		_ = hostLog.Close()
		if err != nil {
			a.recordFailure("hostapd", err)
		}
	}(hostapdCmd)
	if err := a.processSurvives(hostapdCmd, "hostapd"); err != nil {
		return rollback(err)
	}
	dhcpLog, err := os.OpenFile(a.dhcpLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return rollback(err)
	}
	if a.dhcpBackend == "dnsmasq" {
		a.dhcp = exec.Command("dnsmasq", "--keep-in-foreground", "--conf-file="+a.dhcpConfigPath())
	} else {
		a.dhcp = exec.Command("udhcpd", "-f", a.dhcpConfigPath())
	}
	a.dhcp.Stdout = dhcpLog
	a.dhcp.Stderr = dhcpLog
	logCommand(a.dhcp.Path, a.dhcp.Args[1:]...)
	if err := a.dhcp.Start(); err != nil {
		_ = dhcpLog.Close()
		return rollback(fmt.Errorf("%s: %w", a.dhcpBackend, err))
	}
	dhcpCmd := a.dhcp
	go func(cmd *exec.Cmd, backend string) {
		err := cmd.Wait()
		_ = dhcpLog.Close()
		if err != nil {
			a.recordFailure(backend, err)
		}
	}(dhcpCmd, a.dhcpBackend)
	if err := a.processSurvives(dhcpCmd, a.dhcpBackend); err != nil {
		return rollback(err)
	}
	a.running = true
	return nil
}
func (a *app) stop() { a.mu.Lock(); defer a.mu.Unlock(); a.cleanupLocked() }
func (a *app) cleanupLocked() {
	a.stopping = true
	if a.dhcp != nil && a.dhcp.Process != nil {
		logInfof("动作: 停止 DHCP 服务；发送 SIGTERM 给 %s (PID %d)", a.dhcpBackend, a.dhcp.Process.Pid)
		_ = a.dhcp.Process.Signal(syscall.SIGTERM)
	}
	if a.hostapd != nil && a.hostapd.Process != nil {
		logInfof("动作: 停止 Wi-Fi 热点；发送 SIGTERM 给 hostapd (PID %d)", a.hostapd.Process.Pid)
		_ = a.hostapd.Process.Signal(syscall.SIGTERM)
	}
	a.dhcp = nil
	a.hostapd = nil
	if a.cfg != nil {
		a.removeFirewall(*a.cfg)
		if a.oldForward != "" {
			_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(a.oldForward+"\n"), 0644)
		}
		a.restoreNM()
	}
	a.running = false
	a.cfg = nil
	a.clientSamples = make(map[string]trafficSample)
	a.interfaceSample = trafficSample{}
	a.stopping = false
}
func (a *app) prepareLogs() error {
	for _, name := range []string{"hostapd.log", filepath.Base(a.dhcpLogPath())} {
		if err := os.WriteFile(filepath.Join(a.workdir, name), nil, 0600); err != nil {
			return err
		}
	}
	return nil
}
func (a *app) processSurvives(cmd *exec.Cmd, name string) error {
	time.Sleep(350 * time.Millisecond)
	if cmd.ProcessState != nil {
		return fmt.Errorf("%s exited during startup; see logs below", name)
	}
	return nil
}
func (a *app) recordFailure(name string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return
	}
	a.running = false
	a.lastError = fmt.Sprintf("%s exited: %v", name, err)
}
func (a *app) logs() string {
	var parts []string
	for _, name := range []string{"hostapd.log", filepath.Base(a.dhcpLogPath())} {
		if b, err := os.ReadFile(filepath.Join(a.workdir, name)); err == nil && len(b) > 0 {
			if len(b) > 8192 {
				b = b[len(b)-8192:]
			}
			parts = append(parts, "["+name+"]\n"+string(b))
		}
	}
	return strings.Join(parts, "\n")
}
func (a *app) exportConfig() *Config {
	if a.cfg == nil {
		c := a.baseConfig
		return &c
	}
	c := *a.cfg
	c.Workdir = a.workdir
	return &c
}
func (a *app) writeConfigs(c Config) error {
	h := fmt.Sprintf("interface=%s\ndriver=nl80211\nctrl_interface=%s\nctrl_interface_group=0\nssid=%s\nhw_mode=%s\nchannel=%d\ncountry_code=%s\nieee80211d=1\nwmm_enabled=1\nauth_algs=1\nwpa=2\nwpa_passphrase=%s\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\n", c.Interface, a.workdir, c.SSID, map[bool]string{true: "g", false: "a"}[c.Band == "2.4GHz"], c.Channel, c.CountryCode, c.Passphrase)
	if err := os.WriteFile(filepath.Join(a.workdir, "hostapd.conf"), []byte(h), 0600); err != nil {
		return err
	}
	if a.dhcpBackend == "dnsmasq" {
		// The host may already run systemd-resolved, NetworkManager dnsmasq, or
		// another DNS daemon on port 53. This instance is DHCP-only, so disable
		// its DNS listener and pass upstream resolvers to clients instead.
		d := fmt.Sprintf("interface=%s\nbind-interfaces\nport=0\ndhcp-leasefile=%s\ndhcp-range=%s,%s,%s\ndhcp-option=3,%s\ndhcp-option=6,%s\n", c.Interface, a.dhcpLeasePath(), c.DHCPStart, c.DHCPEnd, c.LeaseTime, hostIP(c.GatewayCIDR), strings.Join(upstreamDNS(), ","))
		return os.WriteFile(a.dhcpConfigPath(), []byte(d), 0600)
	}
	lease, _ := time.ParseDuration(c.LeaseTime)
	leaseSeconds := int(lease.Seconds())
	if leaseSeconds < 1 {
		leaseSeconds = 1
	}
	// udhcpd is a DHCP-only daemon. Its lease file has a binary format, so it
	// cannot supply the IP/host-name correlation used by the dnsmasq backend.
	d := fmt.Sprintf("start %s\nend %s\ninterface %s\noption subnet %s\noption router %s\noption dns %s\noption lease %d\nlease_file %s\n", c.DHCPStart, c.DHCPEnd, c.Interface, subnetMask(c.GatewayCIDR), hostIP(c.GatewayCIDR), strings.Join(upstreamDNS(), " "), leaseSeconds, a.dhcpLeasePath())
	return os.WriteFile(a.dhcpConfigPath(), []byte(d), 0600)
}
func (a *app) dhcpConfigPath() string { return filepath.Join(a.workdir, a.dhcpBackend+".conf") }
func (a *app) dhcpLogPath() string    { return filepath.Join(a.workdir, a.dhcpBackend+".log") }
func (a *app) dhcpLeasePath() string  { return filepath.Join(a.workdir, a.dhcpBackend+".leases") }
func upstreamDNS() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var out []string
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "nameserver" {
				ip := net.ParseIP(f[1])
				if ip != nil && !ip.IsLoopback() {
					out = append(out, f[1])
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Domestic public resolvers are the fallback when the host does not expose
	// a usable upstream resolver (for example, resolv.conf only has 127.0.0.53).
	return []string{"223.5.5.5", "114.114.114.114"}
}
func hostIP(c string) string { i, _, _ := net.ParseCIDR(c); return i.String() }
func subnetMask(c string) string {
	_, n, _ := net.ParseCIDR(c)
	return net.IP(n.Mask).String()
}
func (a *app) unmanageNM(iface string) error {
	if _, e := exec.LookPath("nmcli"); e != nil {
		return nil
	}
	out, e := commandOutput("nmcli", "-t", "-f", "GENERAL.STATE", "device", "show", iface)
	if e != nil {
		return nil
	}
	if !strings.Contains(string(out), "unmanaged") {
		if e = run("nmcli", "device", "set", iface, "managed", "no"); e != nil {
			return fmt.Errorf("NetworkManager: %w", e)
		}
		a.nmManaged = true
		a.nmInterface = iface
	}
	return nil
}
func (a *app) restoreNM() {
	if a.nmManaged {
		_ = run("nmcli", "device", "set", a.nmInterface, "managed", "yes")
	}
	a.nmManaged = false
	a.nmInterface = ""
}
func (a *app) addFirewall(c Config) error {
	subnet := subnet(c.GatewayCIDR)
	if a.fw == "nftables" {
		// nft requires a statement separator between chain declarations.  Use a
		// multiline ruleset rather than compacting it into one line: besides being
		// valid for nft, it makes an error printed by nft directly actionable.
		allowUpstream := ""
		if c.AllowUpstreamLAN {
			allowUpstream = fmt.Sprintf("    iifname %q oifname %q ip saddr %s ip daddr %s accept\n", c.UpstreamInterface, c.Interface, c.UpstreamLANCIDR, subnet)
		}
		script := fmt.Sprintf(`table ip sharewifi {
  chain forward {
    type filter hook forward priority 0;
    iifname "%s" oifname "%s" accept
%s
    iifname "%s" oifname "%s" ct state established,related accept
  }
  chain postrouting {
    type nat hook postrouting priority 100;
    oifname "%s" ip saddr %s masquerade
  }
}
`, c.Interface, c.UpstreamInterface, allowUpstream, c.UpstreamInterface, c.Interface, c.UpstreamInterface, subnet)
		return runInput(script, "nft", "-f", "-")
	}
	if err := run("iptables", "-A", "FORWARD", "-i", c.Interface, "-o", c.UpstreamInterface, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := run("iptables", "-A", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	if c.AllowUpstreamLAN {
		if err := run("iptables", "-A", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-s", c.UpstreamLANCIDR, "-d", subnet, "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", c.UpstreamInterface, "-j", "MASQUERADE")
}
func (a *app) removeFirewall(c Config) {
	if a.fw == "nftables" {
		_ = run("nft", "delete", "table", "ip", "sharewifi")
	} else {
		_ = run("iptables", "-D", "FORWARD", "-i", c.Interface, "-o", c.UpstreamInterface, "-j", "ACCEPT")
		_ = run("iptables", "-D", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
		if c.AllowUpstreamLAN {
			_ = run("iptables", "-D", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-s", c.UpstreamLANCIDR, "-d", subnet(c.GatewayCIDR), "-j", "ACCEPT")
		}
		_ = run("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet(c.GatewayCIDR), "-o", c.UpstreamInterface, "-j", "MASQUERADE")
	}
}
func subnet(c string) string { _, n, _ := net.ParseCIDR(c); return n.String() }
func (a *app) clients() ClientStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cfg == nil {
		return ClientStatus{}
	}
	now := time.Now()
	out, e := commandCombinedOutput("hostapd_cli", "-p", a.workdir, "-i", a.cfg.Interface, "all_sta")
	if e != nil {
		return ClientStatus{Error: strings.TrimSpace(string(out))}
	}
	clients := parseStationTraffic(string(out))
	leases := map[string]leaseInfo{}
	if a.dhcpBackend == "dnsmasq" {
		leases = readLeases(a.dhcpLeasePath())
	}
	for i := range clients {
		if lease, ok := leases[strings.ToLower(clients[i].MAC)]; ok {
			clients[i].IP = lease.ip
			clients[i].Name = lease.name
		}
		previous, ok := a.clientSamples[clients[i].MAC]
		clients[i].RXBPS = bytesPerSecond(clients[i].RXBytes, previous.rx, previous.at, now, ok)
		clients[i].TXBPS = bytesPerSecond(clients[i].TXBytes, previous.tx, previous.at, now, ok)
		a.clientSamples[clients[i].MAC] = trafficSample{rx: clients[i].RXBytes, tx: clients[i].TXBytes, at: now}
	}
	interfaceTraffic, e := readInterfaceTraffic(a.cfg.Interface)
	if e != nil {
		return ClientStatus{Clients: clients, Error: e.Error()}
	}
	interfaceTraffic.RXBPS = bytesPerSecond(interfaceTraffic.RXBytes, a.interfaceSample.rx, a.interfaceSample.at, now, !a.interfaceSample.at.IsZero())
	interfaceTraffic.TXBPS = bytesPerSecond(interfaceTraffic.TXBytes, a.interfaceSample.tx, a.interfaceSample.at, now, !a.interfaceSample.at.IsZero())
	a.interfaceSample = trafficSample{rx: interfaceTraffic.RXBytes, tx: interfaceTraffic.TXBytes, at: now}
	return ClientStatus{Clients: clients, Interface: interfaceTraffic, DHCPBackend: a.dhcpBackend}
}

type leaseInfo struct{ ip, name string }

func readLeases(path string) map[string]leaseInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]leaseInfo{}
	}
	leases := make(map[string]leaseInfo)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mac := strings.ToLower(fields[1])
		if _, err := net.ParseMAC(mac); err != nil {
			continue
		}
		name := fields[3]
		if name == "*" {
			name = ""
		}
		leases[mac] = leaseInfo{ip: fields[2], name: name}
	}
	return leases
}

func parseStationTraffic(out string) []ClientTraffic {
	var clients []ClientTraffic
	var current *ClientTraffic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if mac, err := net.ParseMAC(line); err == nil && len(mac) == 6 {
			clients = append(clients, ClientTraffic{MAC: strings.ToUpper(line)})
			current = &clients[len(clients)-1]
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "signal":
			current.Signal = value + " dBm"
		case "tx_rate_info":
			current.TXRate = value
		case "rx_rate_info":
			current.RXRate = value
		case "tx_bytes":
			current.TXBytes, _ = strconv.ParseUint(value, 10, 64)
		case "rx_bytes":
			current.RXBytes, _ = strconv.ParseUint(value, 10, 64)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].MAC < clients[j].MAC })
	return clients
}

func readInterfaceTraffic(iface string) (InterfaceTraffic, error) {
	rx, err := readUint(filepath.Join("/sys/class/net", iface, "statistics/rx_bytes"))
	if err != nil {
		return InterfaceTraffic{}, err
	}
	tx, err := readUint(filepath.Join("/sys/class/net", iface, "statistics/tx_bytes"))
	if err != nil {
		return InterfaceTraffic{}, err
	}
	return InterfaceTraffic{RXBytes: rx, TXBytes: tx}, nil
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func bytesPerSecond(current, previous uint64, then, now time.Time, valid bool) float64 {
	if !valid || current < previous {
		return 0
	}
	seconds := now.Sub(then).Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(current-previous) / seconds
}
func run(name string, args ...string) error {
	o, e := commandCombinedOutput(name, args...)
	if e != nil {
		return fmt.Errorf("%s: %w: %s", name, e, strings.TrimSpace(string(o)))
	}
	return nil
}
func runInput(s, name string, args ...string) error {
	logCommand(name, args...)
	logInfof("command stdin for %s:\n%s", name, s)
	c := exec.Command(name, args...)
	c.Stdin = strings.NewReader(s)
	o, e := c.CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s: %w: %s", name, e, strings.TrimSpace(string(o)))
	}
	return nil
}
func logInfof(format string, args ...any) {
	if infoEnabled {
		log.Printf(format, args...)
	}
}
func logCommand(name string, args ...string) {
	if !infoEnabled {
		return
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}
	log.Printf("动作: %s", commandAction(name, args))
	log.Printf("命令: %s", strings.Join(parts, " "))
}

func commandAction(name string, args []string) string {
	joined := strings.Join(args, " ")
	switch name {
	case "hostapd":
		return "启动 Wi-Fi 热点"
	case "dnsmasq":
		return "启动 DHCP 服务"
	case "udhcpd":
		return "启动 DHCP 服务"
	case "hostapd_cli":
		return "查询已连接 Wi-Fi 设备及流量计数"
	case "nmcli":
		if strings.Contains(joined, "managed no") {
			return "让 NetworkManager 停止管理热点网卡"
		}
		if strings.Contains(joined, "managed yes") {
			return "恢复 NetworkManager 对热点网卡的管理"
		}
		return "查询 NetworkManager 的网卡状态"
	case "nft":
		if strings.Contains(joined, "delete table") {
			return "删除热点防火墙、转发与 NAT 规则"
		}
		return "应用热点防火墙、转发与 NAT 规则"
	case "iptables":
		if strings.Contains(joined, " -D ") || (len(args) > 0 && args[0] == "-D") {
			return "删除热点防火墙或 NAT 规则"
		}
		return "添加热点防火墙、转发或 NAT 规则"
	case "ip":
		if strings.HasPrefix(joined, "link set") {
			return "修改热点无线网卡的链路状态"
		}
		if strings.HasPrefix(joined, "addr flush") {
			return "清除热点网卡现有 IP 地址"
		}
		if strings.HasPrefix(joined, "addr add") {
			return "为热点网卡配置网关 IP 地址"
		}
		if strings.HasPrefix(joined, "route show default") {
			return "检测默认上游网络接口"
		}
		return "查询或配置网络接口与路由"
	case "iw":
		if strings.Contains(joined, "phy") {
			return "检查无线网卡是否支持 AP 模式"
		}
		return "检测无线网络接口"
	default:
		return "执行外部系统命令"
	}
}
func commandOutput(name string, args ...string) ([]byte, error) {
	logCommand(name, args...)
	return exec.Command(name, args...).Output()
}
func commandCombinedOutput(name string, args ...string) ([]byte, error) {
	logCommand(name, args...)
	return exec.Command(name, args...).CombinedOutput()
}
func readTrim(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
func wirelessInterfaces() []string {
	o, e := commandOutput("iw", "dev")
	if e != nil {
		return []string{}
	}
	var r []string
	for _, l := range strings.Split(string(o), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "Interface" {
			r = append(r, f[1])
		}
	}
	sort.Strings(r)
	return r
}
func wirelessCapabilities(iface string) WirelessCapabilities {
	phy, err := wirelessPHY(iface)
	if err == nil {
		out, phyErr := commandOutput("iw", "phy", phy, "info")
		if phyErr == nil {
			caps := parseWirelessCapabilities(string(out))
			caps.Source = "iw phy " + phy + " info"
			if len(caps.Channels24GHz) > 0 || len(caps.Channels5GHz) > 0 {
				return caps
			}
		}
	}
	// Some older iw builds or virtual wireless interfaces cannot answer
	// `iw phy <name> info`. `iw list` is the portable fallback and also makes
	// failures visible instead of silently returning an empty channel list.
	out, listErr := commandOutput("iw", "list")
	if listErr != nil {
		if err != nil {
			return fallbackWirelessCapabilities(fmt.Sprintf("无法读取网卡 %s 的 PHY 信息：%v；iw list 也失败：%v", iface, err, listErr))
		}
		return fallbackWirelessCapabilities(fmt.Sprintf("无法读取无线频段信息：%v", listErr))
	}
	caps := parseWirelessCapabilities(string(out))
	caps.Source = "iw list（兼容回退）"
	if len(caps.Channels24GHz) == 0 && len(caps.Channels5GHz) == 0 {
		return fallbackWirelessCapabilities("iw 已返回信息，但未能解析出 2.4GHz 或 5GHz 信道")
	}
	return caps
}
func fallbackWirelessCapabilities(reason string) WirelessCapabilities {
	return WirelessCapabilities{
		Channels24GHz: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		Channels5GHz:  []int{36, 40, 44, 48, 52, 56, 60, 64, 68, 72, 76, 80, 84, 88, 92, 96, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165, 169, 173, 177, 181},
		Supports5GHz:  true,
		AutoChannel:   false,
		Source:        "内置兼容信道表",
		Error:         reason + "；已使用内置固定信道列表，请手动选择信道。",
	}
}
func parseWirelessCapabilities(output string) WirelessCapabilities {
	var caps WirelessCapabilities
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		freq, channel, ok := parseFrequencyChannel(fields)
		if !ok {
			continue
		}
		if freq >= 2400 && freq < 2500 {
			caps.Channels24GHz = appendUniqueInt(caps.Channels24GHz, channel)
			if !strings.Contains(line, "disabled") && !strings.Contains(line, "no IR") {
				caps.auto24GHz = appendUniqueInt(caps.auto24GHz, channel)
			}
		} else if freq >= 5000 && freq < 5925 {
			caps.Channels5GHz = appendUniqueInt(caps.Channels5GHz, channel)
			if !strings.Contains(line, "disabled") && !strings.Contains(line, "no IR") {
				caps.auto5GHz = appendUniqueInt(caps.auto5GHz, channel)
			}
		}
	}
	sort.Ints(caps.Channels24GHz)
	sort.Ints(caps.Channels5GHz)
	sort.Ints(caps.auto24GHz)
	sort.Ints(caps.auto5GHz)
	caps.Supports5GHz = len(caps.Channels5GHz) > 0
	caps.AutoChannel = len(caps.Channels24GHz) > 0 || len(caps.Channels5GHz) > 0
	return caps
}
func parseFrequencyChannel(fields []string) (int, int, bool) {
	frequency := 0
	for i := 0; i+1 < len(fields); i++ {
		value, err := strconv.ParseFloat(strings.Trim(fields[i], "*[](),"), 64)
		if err == nil && strings.EqualFold(strings.Trim(fields[i+1], "():,"), "MHz") {
			frequency = int(value)
			break
		}
	}
	if frequency == 0 {
		return 0, 0, false
	}
	channel := 0
	for _, field := range fields {
		if strings.HasPrefix(field, "[") && strings.HasSuffix(field, "]") {
			channel, _ = strconv.Atoi(strings.Trim(field, "[]"))
			break
		}
	}
	if channel == 0 {
		channel = frequencyChannel(frequency)
	}
	return frequency, channel, channel > 0
}
func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
func wirelessPHY(iface string) (string, error) {
	out, err := commandOutput("iw", "dev", iface, "info")
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "wiphy" {
				return "phy" + fields[1], nil
			}
		}
	}
	// Fallback for iw versions that do not print wiphy in the per-interface
	// view. The global `iw dev` format groups interfaces under `phy#<number>`.
	all, allErr := commandOutput("iw", "dev")
	if allErr != nil {
		if err != nil {
			return "", err
		}
		return "", allErr
	}
	phy := ""
	for _, line := range strings.Split(string(all), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 && strings.HasPrefix(fields[0], "phy#") {
			phy = "phy" + strings.TrimPrefix(fields[0], "phy#")
			continue
		}
		if len(fields) == 2 && fields[0] == "Interface" && fields[1] == iface && phy != "" {
			return phy, nil
		}
	}
	return "", errors.New("wireless PHY not found")
}
func resolveChannel(iface, band string, requested int) (int, error) {
	caps := wirelessCapabilities(iface)
	candidates := caps.Channels24GHz
	autoCandidates := caps.auto24GHz
	if band == "5GHz" {
		candidates = caps.Channels5GHz
		autoCandidates = caps.auto5GHz
		if !caps.Supports5GHz {
			return 0, errors.New("selected wireless interface does not support 5GHz")
		}
	}
	if requested > 0 {
		if !containsInt(candidates, requested) {
			return 0, fmt.Errorf("channel %d is not available on %s", requested, iface)
		}
		return requested, nil
	}
	if !caps.AutoChannel {
		return 0, errors.New("automatic channel selection is unavailable because wireless capability detection failed; choose a channel manually")
	}
	if len(candidates) == 0 {
		return 0, fmt.Errorf("no usable %s channel was found on %s", band, iface)
	}
	// Prefer channels that the current regulatory state permits initiating
	// radiation on. If none are currently marked usable, retain all hardware
	// channels: hostapd may enable them after applying the configured country.
	if len(autoCandidates) > 0 {
		candidates = autoCandidates
	}
	selected := automaticChannel(iface, band, candidates)
	log.Printf("automatic channel selection: interface=%s band=%s channel=%d", iface, band, selected)
	return selected, nil
}
func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func automaticChannel(iface, band string, candidates []int) int {
	counts := make(map[int]int, len(candidates))
	out, err := commandCombinedOutput("iw", "dev", iface, "scan")
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) != 2 || fields[0] != "freq:" {
				continue
			}
			freq, parseErr := strconv.Atoi(fields[1])
			if parseErr != nil {
				continue
			}
			channel := frequencyChannel(freq)
			if containsInt(candidates, channel) {
				counts[channel]++
			}
		}
	}
	preferred := preferredChannels(band)
	best := candidates[0]
	bestCount := counts[best]
	for _, channel := range candidates[1:] {
		if counts[channel] < bestCount || (counts[channel] == bestCount && preferredRank(channel, preferred) < preferredRank(best, preferred)) {
			best, bestCount = channel, counts[channel]
		}
	}
	return best
}
func frequencyChannel(freq int) int {
	if freq == 2484 {
		return 14
	}
	if freq >= 2412 && freq <= 2472 && (freq-2407)%5 == 0 {
		return (freq - 2407) / 5
	}
	if freq >= 5000 && (freq-5000)%5 == 0 {
		return (freq - 5000) / 5
	}
	return 0
}
func preferredChannels(band string) []int {
	if band == "5GHz" {
		return []int{36, 40, 44, 48}
	}
	return []int{1, 6, 11}
}
func preferredRank(channel int, preferred []int) int {
	for rank, value := range preferred {
		if value == channel {
			return rank
		}
	}
	return len(preferred) + channel
}
func wirelessAPCapable(iface string) bool {
	phy, err := wirelessPHY(iface)
	if err != nil {
		return false
	}
	out, err := commandOutput("iw", "phy", phy, "info")
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "* AP\n") || strings.Contains(string(out), "* AP ")
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func defaultInterface() (string, error) {
	o, e := commandOutput("ip", "route", "show", "default")
	if e != nil {
		return "", e
	}
	f := strings.Fields(string(o))
	for i := range f {
		if f[i] == "dev" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return "", errors.New("no default route found; select an upstream interface")
}

package speedtest

import (
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/table"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/ztelliot/taierspeed-cli/defs"
)

//go:embed province.csv
var ProvinceListByte []byte

type PingJob struct {
	Index  int
	Server defs.Server
}

type PingResult struct {
	Index int
	Ping  float64
}

// SpeedTest is the actual main function that handles the speed test(s)
func SpeedTest(c *cli.Context) error {
	// check for suppressed output flags
	var silent bool
	if c.Bool(defs.OptionSimple) || c.Bool(defs.OptionJSON) || c.Bool(defs.OptionCSV) {
		log.SetLevel(log.WarnLevel)
		silent = true
	}

	// check for debug flag
	if c.Bool(defs.OptionDebug) {
		log.SetLevel(log.DebugLevel)
	}

	// print help
	if c.Bool(defs.OptionHelp) {
		return cli.ShowAppHelp(c)
	}

	// print version
	if c.Bool(defs.OptionVersion) {
		log.SetOutput(os.Stdout)
		log.Warnf("%s %s %s (built on %s)", defs.ProgName, defs.ProgVersion, defs.ProgCommit, defs.BuildDate)
		log.Warn("Powered by TaierSpeed")
		log.Warn("Project: https://github.com/ztelliot/taierspeed-cli")
		log.Warn("Forked: https://github.com/librespeed/speedtest-cli")
		return nil
	}

	if c.String(defs.OptionSource) != "" && c.String(defs.OptionInterface) != "" {
		return fmt.Errorf("incompatible options '%s' and '%s'", defs.OptionSource, defs.OptionInterface)
	}

	// set CSV delimiter
	gocsv.TagSeparator = c.String(defs.OptionCSVDelimiter)

	// if --csv-header is given, print the header and exit (same behavior speedtest-cli)
	if c.Bool(defs.OptionCSVHeader) {
		var rep []defs.Result
		b, _ := gocsv.MarshalBytes(&rep)
		os.Stdout.WriteString(string(b))
		return nil
	}

	if req := c.Int(defs.OptionConcurrent); req <= 0 {
		log.Errorf("Concurrent requests cannot be lower than 1: %d is given", req)
		return errors.New("invalid concurrent requests setting")
	}

	if req := c.Int(defs.OptionPingCount); req <= 0 {
		log.Errorf("Ping count cannot be lower than 1: %d is given", req)
		return errors.New("invalid ping count setting")
	}

	if req := c.Int(defs.OptionUploadSize); req <= 0 {
		log.Errorf("Upload size cannot be lower than 1: %d is given", req)
		return errors.New("invalid upload size setting")
	}

	if req := c.Int(defs.OptionDuration); req > 150 {
		log.Errorf("Duration too long: %d seconds", req)
		return errors.New("invalid duration setting")
	}

	if c.Bool(defs.OptionNoDownload) || c.Bool(defs.OptionNoUpload) {
		log.Warnf("The --%s and --%s options are deprecated and will be removed in the future", defs.OptionNoDownload, defs.OptionNoUpload)
	}

	// HTTP requests timeout
	http.DefaultClient.Timeout = time.Duration(c.Int(defs.OptionTimeout)) * time.Second

	forceIPv4 := c.Bool(defs.OptionIPv4)
	forceIPv6 := c.Bool(defs.OptionIPv6)
	noICMP := c.Bool(defs.OptionNoICMP)

	var network string
	var stack defs.Stack
	switch {
	case forceIPv4:
		network = "ip4"
		stack = defs.StackIPv4
	case forceIPv6:
		network = "ip6"
		stack = defs.StackIPv6
	default:
		network = "ip"
		stack = defs.StackAll
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()

	// bind to source IP address or interface if given, or if ipv4/ipv6 is forced
	if src, iface := c.String(defs.OptionSource), c.String(defs.OptionInterface); src != "" || iface != "" || forceIPv4 || forceIPv6 {
		var localTCPAddr *net.TCPAddr
		if src != "" {
			// first we parse the IP to see if it's valid
			addr, err := net.ResolveIPAddr(network, src)
			if err != nil {
				if strings.Contains(err.Error(), "no suitable address") {
					if forceIPv6 {
						log.Errorf("Address %s is not a valid IPv6 address", src)
					} else {
						log.Errorf("Address %s is not a valid IPv4 address", src)
					}
				} else {
					log.Errorf("Error parsing source IP: %s", err)
				}
				return err
			}

			log.Debugf("Using %s as source IP", src)
			localTCPAddr = &net.TCPAddr{IP: addr.IP}
		}

		var defaultDialer *net.Dialer
		var dialContext func(context.Context, string, string) (net.Conn, error)

		if iface != "" {
			defaultDialer = newInterfaceDialer(iface)
			noICMP = true
		} else {
			defaultDialer = &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
		}

		if localTCPAddr != nil {
			defaultDialer.LocalAddr = localTCPAddr
		}

		switch {
		case forceIPv4:
			dialContext = func(ctx context.Context, network, address string) (conn net.Conn, err error) {
				return defaultDialer.DialContext(ctx, "tcp4", address)
			}
		case forceIPv6:
			dialContext = func(ctx context.Context, network, address string) (conn net.Conn, err error) {
				return defaultDialer.DialContext(ctx, "tcp6", address)
			}
		default:
			dialContext = defaultDialer.DialContext
		}

		// set default HTTP client's Transport to the one that binds the source address
		// this is modified from http.DefaultTransport
		transport.DialContext = dialContext
	}

	if c.Bool(defs.OptionTLSInsecure) {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	http.DefaultClient.Transport = transport

	if c.Bool(defs.OptionCheckUpdate) {
		if latest, err := getVersion(c); err != nil {
			log.Errorf("Error when fetching latest version: %s", err)
		} else {
			if latest.Version != defs.ProgVersion {
				log.Warnf("Current version: %s", defs.ProgVersion)
				log.Warnf("New version available: %s", latest.Version)
				log.Warnf("Download Url: %s", latest.Url)
			} else {
				log.Warn("You are using the latest version")
			}
		}
		return nil
	}

	var ispInfo *defs.IPInfoResponse
	var servers []defs.Server
	var provinceMap map[uint8]defs.ProvinceInfo = nil

	simple := true
	if c.IsSet(defs.OptionServer) || c.IsSet(defs.OptionServerGroup) {
		simple = false
	}
	hasLo := false
	if c.IsSet(defs.OptionServerGroup) {
		for _, s := range c.StringSlice(defs.OptionServerGroup) {
			if strings.Contains(s, "lo") {
				hasLo = true
				break
			}
		}
	}
	if simple || !c.Bool(defs.OptionList) || (c.IsSet(defs.OptionServerGroup) && hasLo) {
		ispInfo, _ = defs.GetIPInfo("")
		if ispInfo != nil {
			if ispInfo.Country == "中国" {
				if ispInfo.Province != "" {
					provinceMap = initProvinceMap()
					ispInfo.ProvId = MatchProvince(ispInfo.Province, &provinceMap)
				}
				if ispInfo.ISP != "" {
					ispInfo.ISPId = MatchISP(ispInfo.ISP)
				}
			}
		}
	}

	// fetch the server list JSON and parse it into the `servers` array
	log.Infof("Retrieving server list")

	excludes := c.StringSlice(defs.OptionExclude)
	if simple {
		if serversT, err := getServerMatch(c, ispInfo, stack); err != nil {
			log.Errorf("Error when fetching server list: %s", err)
			return err
		} else {
			serversT = preprocessServers(stack, serversT, excludes)

			if c.Bool(defs.OptionList) {
				servers = append(servers, serversT...)
			} else {
				log.Debugf("Find %d servers", len(serversT))
				if server, ok := selectServer("", serversT, network, c, noICMP); ok {
					servers = append(servers, server)
				}
			}
		}
	} else {
		var _servers []string
		if c.IsSet(defs.OptionServer) {
			_tmpMap := make(map[string]byte)
			for _, s := range c.StringSlice(defs.OptionServer) {
				_tmpMap[s] = 0
			}
			for s := range _tmpMap {
				_servers = append(_servers, s)
			}
		}

		var _groups []string
		if c.IsSet(defs.OptionServerGroup) {
			if provinceMap == nil {
				provinceMap = initProvinceMap()
			}
			_tmpMap := make(map[string]byte)
			for _, s := range c.StringSlice(defs.OptionServerGroup) {
				sg := strings.Split(s, "@")
				sgp, sgi := "", ""
				switch len(sg) {
				case 1:
					sgp = sg[0]
				case 2:
					sgp, sgi = sg[0], sg[1]
				default:
					continue
				}

				if sgp == "lo" || sgi == "lo" {
					if ispInfo == nil || (sgp == "lo" && ispInfo.Province == "") || (sgi == "lo" && ispInfo.ISP == "") {
						continue
					}
				}

				var province uint8 = 0
				if sgp != "" {
					if sgp == "lo" {
						if ispInfo != nil {
							province = ispInfo.ProvId
						}
					} else {
						for _, p := range provinceMap {
							if p.Code == sgp {
								province = p.ID
								break
							}
						}
					}
					if province == 0 {
						continue
					}
				}

				var isp uint8 = 0
				if sgi != "" {
					if sgi == "lo" {
						if ispInfo != nil {
							isp = ispInfo.ISPId
						}
					} else {
						for _, i := range defs.ISPMap {
							if sgi == strconv.Itoa(int(i.ASN)) || sgi == i.Short {
								isp = i.ID
								break
							}
						}
					}
					if isp == 0 {
						continue
					}
				}

				_tmpMap[fmt.Sprintf("%d@%d", province, isp)] = 0
			}
			for s := range _tmpMap {
				_groups = append(_groups, s)
			}
		}

		if c.IsSet(defs.OptionServerGroup) && len(_groups) == 0 {
			err := errors.New("specified server group(s) not found")
			log.Errorf("Error when selecting server: %s", err)
			return err
		}

		groups, err := getServerList(c, &_servers, &_groups, stack)
		if err != nil {
			log.Errorf("Error when fetching server list: %s", err)
			return err
		}
		for _, g := range groups {
			serversT := preprocessServers(stack, g.Node, excludes)

			if g.Group == "" || c.Bool(defs.OptionList) {
				servers = append(servers, serversT...)
			} else {
				if g.Group != "" {
					if provinceMap == nil {
						provinceMap = initProvinceMap()
					}
					_g := strings.Split(g.Group, "@")
					province, _ := strconv.Atoi(_g[0])
					isp, _ := strconv.Atoi(_g[1])
					logPre := fmt.Sprintf("[%s%s] ", provinceMap[uint8(province)].Short, defs.ISPMap[uint8(isp)].Name)
					log.Debugf("%sFind %d servers", logPre, len(serversT))
					if len(serversT) > 0 {
						if server, ok := selectServer(logPre, serversT, network, c, noICMP); ok {
							servers = append(servers, server)
						}
					}
				}
			}
		}
	}

	log.Debugf("Selected %d servers", len(servers))
	if len(servers) == 0 {
		err := errors.New("specified server(s) not found")
		log.Errorf("Error when selecting server: %s", err)
		return err
	}

	// if --list is given, list all the servers fetched and exit
	if c.Bool(defs.OptionList) {
		if provinceMap == nil {
			provinceMap = initProvinceMap()
		}
		log.Infoln()
		t := table.NewWriter()
		t.SetOutputMirror(os.Stdout)
		t.AppendHeader(table.Row{"ID", "Name", "Prov", "City", "ISP", "v4", "v6"})

		for _, svr := range servers {
			province := svr.Province
			if province == "" && svr.Prov != 0 {
				province = provinceMap[svr.Prov].Short
			}
			v4, v6 := "N", "N"
			if svr.IP != "" {
				v4 = "Y"
			}
			if svr.IPv6 != "" {
				v6 = "Y"
			}
			t.AppendRow(table.Row{svr.ID, svr.Name, province, svr.City, defs.ISPMap[svr.ISP].Name, v4, v6})
		}

		t.Style().Options.DrawBorder = false
		t.Style().Options.SeparateColumns = false
		t.Render()
		return nil
	}

	return doSpeedTest(c, servers, network, silent, noICMP, ispInfo)
}

func initProvinceMap() map[uint8]defs.ProvinceInfo {
	var provinces []defs.ProvinceInfo
	gocsv.UnmarshalBytes(ProvinceListByte, &provinces)
	provinceMap := make(map[uint8]defs.ProvinceInfo)
	for _, p := range provinces {
		provinceMap[p.ID] = p
	}
	return provinceMap
}

func selectServer(logPre string, servers []defs.Server, network string, c *cli.Context, noICMP bool) (defs.Server, bool) {
	if len(servers) > 10 {
		r := rand.New(rand.NewSource(time.Now().Unix()))
		r.Shuffle(len(servers), func(i int, j int) {
			servers[i], servers[j] = servers[j], servers[i]
		})
		servers = servers[:10]
	}

	log.Infof("%sSelecting the fastest server based on ping", logPre)

	var wg sync.WaitGroup
	jobs := make(chan PingJob, len(servers))
	results := make(chan PingResult, len(servers))
	done := make(chan struct{})

	pingList := make(map[int]float64)

	// spawn 10 concurrent pingers
	for i := 0; i < 10; i++ {
		go pingWorker(jobs, results, &wg, c.String(defs.OptionSource), network, noICMP)
	}

	// send ping jobs to workers
	for idx, server := range servers {
		wg.Add(1)
		jobs <- PingJob{Index: idx, Server: server}
	}

	go func() {
		wg.Wait()
		close(done)
	}()

Loop:
	for {
		select {
		case result := <-results:
			pingList[result.Index] = result.Ping
		case <-done:
			break Loop
		}
	}

	if len(pingList) == 0 {
		log.Infof("%sNo server is currently available", logPre)
		return defs.Server{}, false
	}

	// get the fastest server's index in the `servers` array
	var serverIdx int
	minPing := math.MaxFloat64
	for idx, ping := range pingList {
		if ping > 0 && ping <= minPing {
			serverIdx = idx
		}
	}

	// do speed test on the server
	log.Debugf("%sSelected %s (%s)", logPre, servers[serverIdx].Name, servers[serverIdx].ID)
	return servers[serverIdx], true
}

func pingWorker(jobs <-chan PingJob, results chan<- PingResult, wg *sync.WaitGroup, srcIp, network string, noICMP bool) {
	for {
		job := <-jobs
		server := job.Server

		// check the server is up by accessing the ping URL and checking its returned value == empty and status code == 200
		if server.IsUp() {
			// skip ICMP if option given
			server.NoICMP = noICMP

			// if server is up, get ping
			ping, _, err := server.ICMPPingAndJitter(1, srcIp, network)
			if err != nil {
				log.Debugf("Can't ping server %s (%s), skipping", server.Name, server.ID)
				wg.Done()
				return
			}
			// return result
			results <- PingResult{Index: job.Index, Ping: ping}
			wg.Done()
		} else {
			log.Debugf("Server %s (%s) seems down, skipping", server.Name, server.ID)
			wg.Done()
		}
	}
}

// preprocessServers makes some needed modifications to the servers fetched
func preprocessServers(stack defs.Stack, servers []defs.Server, excludes []string) []defs.Server {
	// exclude servers from --exclude
	var ret []defs.Server
	for _, server := range servers {
		if len(excludes) > 0 && contains(excludes, server.ID) {
			continue
		}
		if server.IPv6 != "" && stack != defs.StackIPv4 {
			server.Target = server.IPv6
		} else if server.IP != "" && stack != defs.StackIPv6 {
			server.Target = server.IP
		}
		if server.Type == defs.StaticFile {
			if server.Target == "" {
				network := "ip"
				if stack == defs.StackIPv4 {
					network = "ip4"
				} else if stack == defs.StackIPv6 {
					network = "ip6"
				}
				server.Target = resolveHost(network, server.Host)
			}
			if server.Target != "" {
				if info, err := defs.GetIPInfo(server.Target); err == nil && info != nil {
					if info.ISP != "" {
						info.ISPId = MatchISP(info.ISP)
					}
					server.Province = info.Province
					server.City = info.City
					server.ISP = info.ISPId
				}
			}
		}
		if server.Target == "" {
			continue
		}
		ret = append(ret, server)
	}
	return ret
}

// contains is a helper function to check if a string is in a string array
func contains(arr []string, val string) bool {
	for _, v := range arr {
		if v == val {
			return true
		}
	}
	return false
}

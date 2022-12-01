package speedtest

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/briandowns/spinner"
	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/librespeed/speedtest-cli/defs"
	"github.com/librespeed/speedtest-cli/report"
)

const (
	// the default ping count for measuring ping and jitter
	pingCount = 10
)

func enQueue(s defs.Server) string {
	time.Local, _ = time.LoadLocation("Asia/Chongqing")
	ts := strconv.Itoa(int(time.Now().Local().Unix()))

	md5Ctx := md5.New()
	md5Ctx.Write([]byte("model=Android&imei=" + defs.IMEI + "&stime=" + ts))
	token := hex.EncodeToString(md5Ctx.Sum(nil))

	url := fmt.Sprintf("http://%s:%s/speed/dovalid?key=&flag=true&bandwidth=200&model=Android&imei=%s&time=%s&token=%s", s.IP, s.Port, defs.IMEI, ts, token)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debugf("Failed when creating HTTP request: %s", err)
		return ""
	}
	req.Header.Set("User-Agent", defs.UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("Failed when making HTTP request: %s", err)
		return ""
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed when reading HTTP response: %s", err)
		return ""
	}

	if len(b) <= 0 {
		return ""
	}

	return string(b)[2:]
}

func deQueue(s defs.Server, key string) bool {
	url := fmt.Sprintf("http://%s:%s/speed/dovalid?key=%s", s.IP, s.Port, key)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debugf("Failed when creating HTTP request: %s", err)
		return false
	}
	req.Header.Set("User-Agent", defs.UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("Failed when making HTTP request: %s", err)
		return false
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed when reading HTTP response: %s", err)
		return false
	}

	if len(b) <= 0 {
		return false
	}

	return true
}

// doSpeedTest is where the actual speed test happens
func doSpeedTest(c *cli.Context, servers []defs.Server, silent bool, ispInfo *defs.IPInfoResponse) error {
	if serverCount := len(servers); serverCount > 1 {
		log.Infof("Testing against %d servers", serverCount)
	}

	var reps_json []report.JSONReport
	var reps_csv []report.CSVReport

	// fetch current user's IP info
	for _, currentServer := range servers {

		log.Infof("Server:\t\t%s [%s]", currentServer.Name, currentServer.IP)

		token := enQueue(currentServer)

		if len(token) > 0 {
			// get ping and jitter value
			var pb *spinner.Spinner
			if !silent {
				pb = spinner.New(spinner.CharSets[11], 100*time.Millisecond)
				pb.Prefix = "Pinging server...  "
				pb.Start()
			}

			// skip ICMP if option given
			currentServer.NoICMP = c.Bool(defs.OptionNoICMP)

			p, jitter, err := currentServer.ICMPPingAndJitter(pingCount, c.String(defs.OptionSource))
			if err != nil {
				log.Errorf("Failed to get ping and jitter: %s", err)
				return err
			}

			if pb != nil {
				pb.FinalMSG = fmt.Sprintf("Latency:\t%.2f ms (%.2f ms jitter)\n", p, jitter)
				pb.Stop()
			}

			// get download value
			var downloadValue float64
			var bytesRead int
			if c.Bool(defs.OptionNoDownload) {
				log.Info("Download test is disabled")
			} else {
				download, br, err := currentServer.Download(silent, c.Bool(defs.OptionBytes), c.Bool(defs.OptionMebiBytes), c.Int(defs.OptionConcurrent), c.Int(defs.OptionChunks), time.Duration(c.Int(defs.OptionDuration))*time.Second, token)
				if err != nil {
					log.Errorf("Failed to get download speed: %s", err)
					return err
				}
				downloadValue = download
				bytesRead = br
			}

			// get upload value
			var uploadValue float64
			var bytesWritten int
			if c.Bool(defs.OptionNoUpload) {
				log.Info("Upload test is disabled")
			} else {
				upload, bw, err := currentServer.Upload(c.Bool(defs.OptionNoPreAllocate), silent, c.Bool(defs.OptionBytes), c.Bool(defs.OptionMebiBytes), c.Int(defs.OptionConcurrent), c.Int(defs.OptionUploadSize), time.Duration(c.Int(defs.OptionDuration))*time.Second, token)
				if err != nil {
					log.Errorf("Failed to get upload speed: %s", err)
					return err
				}
				uploadValue = upload
				bytesWritten = bw
			}

			deQueue(currentServer, token)

			// print result if --simple is given
			if c.Bool(defs.OptionSimple) {
				if c.Bool(defs.OptionBytes) {
					useMebi := c.Bool(defs.OptionMebiBytes)
					log.Warnf("Latency:\t\t%.2f ms (%.2f ms jitter)\nDownload:\t%s\nUpload:\t\t%s", p, jitter, humanizeMbps(downloadValue, useMebi), humanizeMbps(uploadValue, useMebi))
				} else {
					log.Warnf("Latency:\t\t%.2f ms (%.2f ms jitter)\nDownload:\t%.2f Mbps\nUpload:\t\t%.2f Mbps", p, jitter, downloadValue, uploadValue)
				}
			}

			// check for --csv or --json. the program prioritize the --csv before the --json. this is the same behavior as speedtest-cli
			if c.Bool(defs.OptionCSV) {
				// print csv if --csv is given
				var rep report.CSVReport
				rep.Timestamp = time.Now()

				rep.Name = currentServer.Name
				rep.Address = currentServer.IP
				rep.Ping = math.Round(p*100) / 100
				rep.Jitter = math.Round(jitter*100) / 100
				rep.Download = math.Round(downloadValue*100) / 100
				rep.Upload = math.Round(uploadValue*100) / 100
				rep.IP = ispInfo.IP

				reps_csv = append(reps_csv, rep)
			} else if c.Bool(defs.OptionJSON) {
				// print json if --json is given
				var rep report.JSONReport
				rep.Timestamp = time.Now()

				rep.Ping = math.Round(p*100) / 100
				rep.Jitter = math.Round(jitter*100) / 100
				rep.Download = math.Round(downloadValue*100) / 100
				rep.Upload = math.Round(uploadValue*100) / 100
				rep.BytesReceived = bytesRead
				rep.BytesSent = bytesWritten

				rep.Server.Name = currentServer.Name
				rep.Server.IP = currentServer.IP

				rep.Client = *ispInfo

				reps_json = append(reps_json, rep)
			}
		} else {
			log.Infof("Selected server %s (%s) is not responding at the moment, try again later", currentServer.Name, currentServer.IP)
		}

		//add a new line after each test if testing multiple servers
		if len(servers) > 1 && !silent {
			log.Warn()
		}
	}

	// check for --csv or --json. the program prioritize the --csv before the --json. this is the same behavior as speedtest-cli
	if c.Bool(defs.OptionCSV) {
		var buf bytes.Buffer
		if err := gocsv.MarshalWithoutHeaders(&reps_csv, &buf); err != nil {
			log.Errorf("Error generating CSV report: %s", err)
		} else {
			os.Stdout.WriteString(buf.String())
		}
	} else if c.Bool(defs.OptionJSON) {
		if b, err := json.Marshal(&reps_json); err != nil {
			log.Errorf("Error generating JSON report: %s", err)
		} else {
			os.Stdout.Write(b[:])
		}
	}

	return nil
}

func humanizeMbps(mbps float64, useMebi bool) string {
	val := mbps / 8
	var base float64 = 1000
	if useMebi {
		base = 1024
	}

	if val < 1 {
		if kb := val * base; kb < 1 {
			return fmt.Sprintf("%.2f bytes/s", kb*base)
		} else {
			return fmt.Sprintf("%.2f KB/s", kb)
		}
	} else if val > base {
		return fmt.Sprintf("%.2f GB/s", val/base)
	} else {
		return fmt.Sprintf("%.2f MB/s", val)
	}
}

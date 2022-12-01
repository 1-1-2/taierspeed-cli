package report

import (
	"github.com/librespeed/speedtest-cli/defs"
	"time"
)

// JSONReport represents the output data fields in a JSON file
type JSONReport struct {
	Timestamp     time.Time           `json:"timestamp"`
	Server        Server              `json:"server"`
	Client        defs.IPInfoResponse `json:"client"`
	BytesSent     int                 `json:"bytes_sent"`
	BytesReceived int                 `json:"bytes_received"`
	Ping          float64             `json:"ping"`
	Jitter        float64             `json:"jitter"`
	Upload        float64             `json:"upload"`
	Download      float64             `json:"download"`
}

// Server represents the speed test server's information
type Server struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

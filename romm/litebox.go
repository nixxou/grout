package romm

import (
	"encoding/json"
	"fmt"
	"grout/version"
	"net/http"
	"net/url"
	"sync"
)

// LiteBoxClientHeader is the handshake a LiteBox server (RommLiteBoxApi.ClientHeader) expects on
// EVERY request from a client that understands its extensions. Without it the server behaves as a
// stock RomM: the /api/litebox/... routes answer 404 like any unknown path, and no additive field
// is emitted. A real RomM server simply ignores an unknown header, so this is sent unconditionally.
const LiteBoxClientHeader = "X-LiteBox-Client"

// LiteBoxMainVersion is the path segment naming a game's own ROM in the versions/roms route — a
// segment cannot be empty, so the server reads this literal as "" internally.
const LiteBoxMainVersion = "main"

// LiteBoxFeatureVersionSwitch is the capabilities flag for the versions/roms/pin routes.
const LiteBoxFeatureVersionSwitch = "version-switch"

// LiteBoxClientValue identifies this build to a LiteBox server.
func LiteBoxClientValue() string {
	return "grout/" + version.Get().Version
}

// ApplyLiteBoxHeader stamps the handshake on a request bound for the RomM host.
func ApplyLiteBoxHeader(req *http.Request) {
	req.Header.Set(LiteBoxClientHeader, LiteBoxClientValue())
}

// LiteBoxCapabilities is what /api/litebox/capabilities answers. Unlike the rest of the RomM
// contract these payloads are camelCase, exactly as LiteBox sends them.
type LiteBoxCapabilities struct {
	LiteBox  bool     `json:"liteBox"`
	Features []string `json:"features"`
}

func (c LiteBoxCapabilities) Has(feature string) bool {
	for _, f := range c.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// LiteBoxVersion is one top-level choice for a game: its own file (empty AppID) or one Additional
// Application. Eligible is the server's own verdict (the rule its rom_id generation uses): an
// eligible version is an archive whose roms are chosen one by one on a second screen, never pinned
// whole. RomCount is known only for an eligible version the server has already analysed. RomID is
// the id a whole-file version already has, nil until something pins it (or when eligible, since
// only its roms carry ids). IsCurrent says this device is served from it today.
type LiteBoxVersion struct {
	AppID     string `json:"appId"`
	Label     string `json:"label"`
	FileName  string `json:"fileName"`
	Size      int64  `json:"size"`
	Eligible  bool   `json:"eligible"`
	RomCount  *int   `json:"romCount"`
	RomID     *int   `json:"romId"`
	IsPinned  bool   `json:"isPinned"`
	IsCurrent bool   `json:"isCurrent"`
}

// LiteBoxRomEntry is one rom inside a single eligible version's archive. Path is the
// archive-relative entry path, opaque to Grout — round-tripped verbatim into the pin request. The
// flags are the desktop picker's own columns.
type LiteBoxRomEntry struct {
	Path         string `json:"path"`
	Label        string `json:"label"`
	FileName     string `json:"fileName"`
	Size         int64  `json:"size"`
	RomID        *int   `json:"romId"`
	IsPinned     bool   `json:"isPinned"`
	IsCurrent    bool   `json:"isCurrent"`
	IsFavorite   bool   `json:"isFavorite"`
	IsLastPlayed bool   `json:"isLastPlayed"`
	Score        int    `json:"score"`
	HasRa        bool   `json:"hasRa"`
}

// LiteBoxRomsInVersion is the second screen's payload: which version, and which archive file, the
// roms come from.
type LiteBoxRomsInVersion struct {
	AppID           string            `json:"appId"`
	VersionLabel    string            `json:"versionLabel"`
	ArchiveFileName string            `json:"archiveFileName"`
	Roms            []LiteBoxRomEntry `json:"roms"`
}

// LiteBoxPinRequest is the body of POST .../pin. Unpin alone means "follow the default again";
// otherwise AppID/Path name the choice, both blank meaning the game's own ROM as a whole.
type LiteBoxPinRequest struct {
	Unpin bool   `json:"unpin"`
	AppID string `json:"appId"`
	Path  string `json:"path"`
}

// LiteBoxPinResponse carries the rom_id this client is served for the game after the pin — the
// identity to resync against, since it usually differs from the one the pin was requested on.
type LiteBoxPinResponse struct {
	OK    bool `json:"ok"`
	RomID int  `json:"romId"`
}

// The capabilities probe is remembered per base URL for the life of the process: one request per
// server, including the 404 an official RomM answers. Only a real HTTP answer is kept — a network
// failure is never cached, otherwise one offline probe would hide the feature until restart.
var liteBoxProbe = struct {
	sync.Mutex
	answers map[string]LiteBoxCapabilities
}{answers: make(map[string]LiteBoxCapabilities)}

// GetLiteBoxCapabilities probes the server once. Any HTTP status other than 2xx (404/501 from a
// stock RomM) reads as "not a LiteBox" — an empty answer, never an error the caller has to react
// to. Built by hand rather than through doRequest so a transport failure can be told apart from a
// status the server actually answered: only the latter is worth remembering.
func (c *Client) GetLiteBoxCapabilities() LiteBoxCapabilities {
	liteBoxProbe.Lock()
	if cached, ok := liteBoxProbe.answers[c.baseURL]; ok {
		liteBoxProbe.Unlock()
		return cached
	}
	liteBoxProbe.Unlock()

	req, err := http.NewRequest("GET", c.baseURL+endpointLiteBoxCapabilities, nil)
	if err != nil {
		return LiteBoxCapabilities{}
	}
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	ApplyLiteBoxHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return LiteBoxCapabilities{}
	}
	defer resp.Body.Close()

	var caps LiteBoxCapabilities
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			caps = LiteBoxCapabilities{}
		}
	}

	liteBoxProbe.Lock()
	liteBoxProbe.answers[c.baseURL] = caps
	liteBoxProbe.Unlock()
	return caps
}

// SupportsLiteBoxVersionSwitch says whether the versions/roms/pin routes exist on this server.
func (c *Client) SupportsLiteBoxVersionSwitch() bool {
	return c.GetLiteBoxCapabilities().Has(LiteBoxFeatureVersionSwitch)
}

// GetLiteBoxVersions lists every version a game could be served as.
func (c *Client) GetLiteBoxVersions(romID int) ([]LiteBoxVersion, error) {
	var versions []LiteBoxVersion
	err := c.doRequest("GET", fmt.Sprintf(endpointLiteBoxVersions, romID), nil, nil, &versions)
	return versions, err
}

// GetLiteBoxRomsInVersion lists the roms inside ONE eligible version's archive. Only ever called
// for a version GetLiteBoxVersions said was eligible — the server answers 400 for one served whole.
func (c *Client) GetLiteBoxRomsInVersion(romID int, appID string) (LiteBoxRomsInVersion, error) {
	segment := appID
	if segment == "" {
		segment = LiteBoxMainVersion
	}
	var result LiteBoxRomsInVersion
	err := c.doRequest("GET", fmt.Sprintf(endpointLiteBoxVersionRoms, romID, url.PathEscape(segment)), nil, nil, &result)
	return result, err
}

// PinLiteBoxVersion locks this client onto one specific file. Both blank means the game's own ROM
// as a whole; path is required for an eligible version and refused for one served whole.
func (c *Client) PinLiteBoxVersion(romID int, appID, path string) (LiteBoxPinResponse, error) {
	var result LiteBoxPinResponse
	body := LiteBoxPinRequest{AppID: appID, Path: path}
	err := c.doRequest("POST", fmt.Sprintf(endpointLiteBoxPin, romID), nil, body, &result)
	return result, err
}

// UnpinLiteBoxVersion releases this client back to the game's default.
func (c *Client) UnpinLiteBoxVersion(romID int) (LiteBoxPinResponse, error) {
	var result LiteBoxPinResponse
	err := c.doRequest("POST", fmt.Sprintf(endpointLiteBoxPin, romID), nil, LiteBoxPinRequest{Unpin: true}, &result)
	return result, err
}

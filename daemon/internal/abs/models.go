package abs

import "encoding/json"

// Only the fields this client uses are modelled. Audiobookshelf returns a lot
// more; unknown fields are ignored by encoding/json.

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Token        string `json:"token"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// Me is GET /api/me; only the progress list is used.
type Me struct {
	ID            string          `json:"id"`
	Username      string          `json:"username"`
	MediaProgress []MediaProgress `json:"mediaProgress"`
}

type loginResponse struct {
	User User `json:"user"`
}

type Library struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
}

type librariesResponse struct {
	Libraries []Library `json:"libraries"`
}

type NamedRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Sequence string `json:"sequence,omitempty"`
}

type Metadata struct {
	Title         string     `json:"title"`
	Subtitle      string     `json:"subtitle"`
	AuthorName    string     `json:"authorName"`
	Authors       []NamedRef `json:"authors"`
	NarratorName  string     `json:"narratorName"`
	Narrators     []string   `json:"narrators"`
	SeriesName    string     `json:"seriesName"`
	Series        seriesList `json:"series"`
	Description   string     `json:"description"`
	PublishedYear string     `json:"publishedYear"`
	Publisher     string     `json:"publisher"`
	Genres        []string   `json:"genres"`
}

// seriesList accepts both the expanded form (array of refs) and the minified
// form some endpoints return (a single object).
type seriesList []NamedRef

func (s *seriesList) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		var arr []NamedRef
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	var one NamedRef
	if err := json.Unmarshal(b, &one); err != nil {
		return nil
	}
	*s = seriesList{one}
	return nil
}

type Chapter struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

type FileMetadata struct {
	Filename string `json:"filename"`
	Ext      string `json:"ext"`
	Size     int64  `json:"size"`
}

type AudioTrack struct {
	Index       int          `json:"index"`
	StartOffset float64      `json:"startOffset"`
	Duration    float64      `json:"duration"`
	Title       string       `json:"title"`
	ContentURL  string       `json:"contentUrl"`
	MimeType    string       `json:"mimeType"`
	Metadata    FileMetadata `json:"metadata"`
}

type Media struct {
	Metadata    Metadata     `json:"metadata"`
	Duration    float64      `json:"duration"`
	Chapters    []Chapter    `json:"chapters"`
	Tracks      []AudioTrack `json:"tracks"`
	NumTracks   int          `json:"numTracks"`
	NumChapters int          `json:"numChapters"`
	CoverPath   string       `json:"coverPath"`
}

type MediaProgress struct {
	ID            string  `json:"id"`
	LibraryItemID string  `json:"libraryItemId"`
	Duration      float64 `json:"duration"`
	Progress      float64 `json:"progress"`
	CurrentTime   float64 `json:"currentTime"`
	IsFinished    bool    `json:"isFinished"`
	LastUpdate    int64   `json:"lastUpdate"`
}

type LibraryItem struct {
	ID                string         `json:"id"`
	LibraryID         string         `json:"libraryId"`
	MediaType         string         `json:"mediaType"`
	Media             Media          `json:"media"`
	UserMediaProgress *MediaProgress `json:"userMediaProgress"`
	UpdatedAt         int64          `json:"updatedAt"`
}

type ItemsPage struct {
	Results []LibraryItem `json:"results"`
	Total   int           `json:"total"`
	Limit   int           `json:"limit"`
	Page    int           `json:"page"`
}

type searchResponse struct {
	Book []struct {
		LibraryItem LibraryItem `json:"libraryItem"`
	} `json:"book"`
}

type itemsInProgressResponse struct {
	LibraryItems []LibraryItem `json:"libraryItems"`
}

type DeviceInfo struct {
	DeviceID      string `json:"deviceId"`
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
	Manufacturer  string `json:"manufacturer"`
	Model         string `json:"model"`
}

type PlaySession struct {
	ID            string       `json:"id"`
	LibraryItemID string       `json:"libraryItemId"`
	DisplayTitle  string       `json:"displayTitle"`
	DisplayAuthor string       `json:"displayAuthor"`
	CurrentTime   float64      `json:"currentTime"`
	Duration      float64      `json:"duration"`
	AudioTracks   []AudioTrack `json:"audioTracks"`
	Chapters      []Chapter    `json:"chapters"`
}

// LocalSession is the payload for POST /api/session/local, used to report
// listening that happened while the server could not be reached.
type LocalSession struct {
	ID            string     `json:"id"`
	LibraryItemID string     `json:"libraryItemId"`
	MediaType     string     `json:"mediaType"`
	DisplayTitle  string     `json:"displayTitle"`
	DisplayAuthor string     `json:"displayAuthor"`
	Duration      float64    `json:"duration"`
	PlayMethod    int        `json:"playMethod"`
	MediaPlayer   string     `json:"mediaPlayer"`
	DeviceInfo    DeviceInfo `json:"deviceInfo"`
	StartTime     float64    `json:"startTime"`
	CurrentTime   float64    `json:"currentTime"`
	TimeListening float64    `json:"timeListening"`
	StartedAt     int64      `json:"startedAt"`
	UpdatedAt     int64      `json:"updatedAt"`
}

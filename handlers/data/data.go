package handlers

import (
	"bytes"
	"errors"
	"instafix/utils"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/PurpleSec/escape"
	_ "github.com/bdandy/go-socks4"
	"github.com/cockroachdb/pebble"
	"github.com/kelindar/binary"
	"github.com/rs/zerolog/log"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	"golang.org/x/net/html"
)

var ProxyFilePath string

var gjsonNil = gjson.Result{}

var timeout = 10 * time.Second

var (
	ErrNotFound = errors.New("post not found")
)

type Media struct {
	TypeName []byte
	URL      []byte
}

type InstaData struct {
	PostID   []byte
	Username []byte
	Caption  []byte
	Medias   []Media
}

func (i *InstaData) GetData(postID string) error {
	cacheInstaData, closer, err := DB.Get(utils.S2B(postID))
	if err != nil && err != pebble.ErrNotFound {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to get data from cache")
		return err
	}

	if len(cacheInstaData) > 0 {
		err := binary.Unmarshal(cacheInstaData, i)
		closer.Close()
		if err != nil {
			return err
		}
		log.Info().Str("postID", postID).Msg("Data parsed from cache")
		return nil
	}

	data, err := getData(postID)
	if err != nil {
		if err != ErrNotFound {
			log.Error().Str("postID", postID).Err(err).Msg("Failed to get data from Instagram")
		} else {
			log.Warn().Str("postID", postID).Err(err).Msg("Post not found; err getData")
		}
		return err
	}

	item := data.Get("shortcode_media")
	if !item.Exists() {
		return errors.New("shortcode_media not found")
	}

	media := []gjson.Result{item}
	if item.Get("edge_sidecar_to_children").Exists() {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	}

	i.PostID = utils.S2B(postID)

	// Get username
	i.Username = []byte(item.Get("owner.username").String())

	// Get caption
	i.Caption = bytes.TrimSpace([]byte(item.Get("edge_media_to_caption.edges.0.node.text").String()))

	// Get medias
	i.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		}
		i.Medias = append(i.Medias, Media{
			TypeName: []byte(m.Get("__typename").String()),
			URL:      []byte(mediaURL.String()),
		})
	}

	bb, err := binary.Marshal(i)
	if err != nil {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to marshal data")
		return err
	}

	batch := DB.NewBatch()
	// Write cache to DB
	if err := batch.Set(utils.S2B(postID), bb, pebble.Sync); err != nil {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to save data to cache")
		return err
	}

	// Write expire to DB
	expTime := strconv.FormatInt(time.Now().Add(24*time.Hour).UnixNano(), 10)
	if err := batch.Set(append([]byte("exp-"), expTime...), utils.S2B(postID), pebble.Sync); err != nil {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to save data to cache")
		return err
	}

	// Commit batch
	if err := batch.Commit(pebble.Sync); err != nil {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to commit batch")
		return err
	}
	return nil
}

func getData(postID string) (gjson.Result, error) {
	client := &fasthttp.Client{
		ReadBufferSize:     16 * 1024,
		MaxConnsPerHost:    1024,
		MaxConnWaitTimeout: 5 * time.Second,
	}
	socksProxy := getRandomProxy()
	if socksProxy != "" {
		client.Dial = fasthttpproxy.FasthttpSocksDialer(socksProxy)
	}

	req, res := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer func() {
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(res)
	}()

	req.Header.SetMethod("GET")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "close")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36")
	req.SetRequestURI("https://www.instagram.com/p/" + postID + "/embed/captioned/")

	var err error
	for retries := 0; retries < 3; retries++ {
		err := client.DoTimeout(req, res, timeout)
		if err == nil && len(res.Body()) > 0 {
			break
		}
	}

	// Pattern matching using LDE
	l := &Line{}

	// TimeSliceImpl
	ldeMatch := false
	for _, line := range bytes.Split(res.Body(), []byte("\n")) {
		// Check if line contains TimeSliceImpl
		ldeMatch, _ = l.Extract(line)
	}

	if ldeMatch {
		lexer := js.NewLexer(parse.NewInputBytes(l.GetTimeSliceImplValue()))
		for {
			tt, text := lexer.Next()
			if tt == js.ErrorToken {
				break
			}
			if tt == js.StringToken && bytes.Contains(text, []byte("shortcode_media")) {
				// Strip quotes from start and end
				text = text[1 : len(text)-1]
				unescapeData := utils.UnescapeJSONString(utils.B2S(text))
				if !gjson.Valid(unescapeData) {
					log.Error().Str("postID", postID).Err(err).Msg("Failed to parse data from TimeSliceImpl")
					return gjsonNil, err
				}
				timeSlice := gjson.Parse(unescapeData)
				log.Info().Str("postID", postID).Msg("Data parsed from TimeSliceImpl")
				return timeSlice.Get("gql_data"), nil
			}
		}
	}

	// Parse embed HTML
	embedHTML, err := parseEmbedHTML(res.Body())
	if err != nil {
		log.Error().Str("postID", postID).Err(err).Msg("Failed to parse data from ParseEmbedHTML")
		return gjsonNil, err
	}

	embedHTMLData := gjson.Parse(embedHTML)

	smedia := embedHTMLData.Get("shortcode_media")
	videoBlocked := smedia.Get("video_blocked").Bool()
	username := smedia.Get("owner.username").String()

	// Scrape from GraphQL API
	if videoBlocked || len(username) == 0 {
		gqlValue, err := parseGQLData(postID, req, res)
		if err != nil {
			log.Error().Str("postID", postID).Err(err).Msg("Failed to parse data from parseGQLData")
			return gjsonNil, err
		}
		gqlData := gjson.Parse(utils.B2S(gqlValue))
		if gqlData.Get("data").Exists() {
			log.Info().Str("postID", postID).Msg("Data parsed from parseGQLData")
			return gqlData.Get("data"), nil
		}
	}

	// Failed to scrape from Embed
	if len(username) == 0 {
		return gjsonNil, ErrNotFound
	}

	log.Info().Str("postID", postID).Msg("Data parsed from ParseEmbedHTML")
	return embedHTMLData, nil
}

// Taken from https://github.com/PuerkitoBio/goquery
// Modified to add new line every <br>
func gqTextNewLine(s *goquery.Selection) string {
	// Slightly optimized vs calling Each: no single selection object created
	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			// Keep newlines and spaces, like jQuery
			sb.WriteString(n.Data)
		} else if n.Type == html.ElementNode && n.Data == "br" {
			sb.WriteString("\n")
		}
		if n.FirstChild != nil {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
	}
	for _, n := range s.Nodes {
		f(n)
	}
	return sb.String()
}

func parseEmbedHTML(embedHTML []byte) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(embedHTML))
	if err != nil {
		return "", err
	}

	// Get media URL
	typename := "GraphImage"
	embedMedia := doc.Find(".EmbeddedMediaImage")
	if embedMedia.Length() == 0 {
		typename = "GraphVideo"
		embedMedia = doc.Find(".EmbeddedMediaVideo")
	}
	mediaURL, _ := embedMedia.Attr("src")

	// Get username
	username := doc.Find(".UsernameText").Text()

	// Get caption
	captionComments := doc.Find(".CaptionComments")
	if captionComments.Length() > 0 {
		captionComments.Remove()
	}
	captionUsername := doc.Find(".CaptionUsername")
	if captionUsername.Length() > 0 {
		captionUsername.Remove()
	}
	caption := gqTextNewLine(doc.Find(".Caption"))

	// Check if contains WatchOnInstagram
	videoBlocked := strconv.FormatBool(bytes.Contains(embedHTML, []byte("WatchOnInstagram")))

	// Totally safe 100% valid JSON 👍
	return `{
		"shortcode_media": {
			"owner": {"username": "` + username + `"},
			"node": {"__typename": "` + typename + `", "display_url": "` + mediaURL + `"},
			"edge_media_to_caption": {"edges": [{"node": {"text": ` + escape.JSON(caption) + `}}]},
			"dimensions": {"height": null, "width": null},
			"video_blocked": ` + videoBlocked + `
		}
	}`, nil
}

func parseGQLData(postID string, req *fasthttp.Request, res *fasthttp.Response) ([]byte, error) {
	client := &fasthttp.Client{
		ReadBufferSize:     16 * 1024,
		MaxConnsPerHost:    1024,
		MaxConnWaitTimeout: 5 * time.Second,
	}
	socksProxy := getRandomProxy()
	if socksProxy != "" {
		client.Dial = fasthttpproxy.FasthttpSocksDialer(socksProxy)
	}

	req.Reset()
	res.Reset()

	req.Header.SetMethod("GET")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "close")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://www.instagram.com/p/"+postID+"/")

	req.SetRequestURI("https://www.instagram.com/graphql/query/")
	req.URI().QueryArgs().Add("query_hash", "b3055c01b4b222b8a47dc12b090e4e64")
	req.URI().QueryArgs().Add("variables", "{\"shortcode\":\""+postID+"\"}")

	if err := client.DoTimeout(req, res, timeout); err != nil {
		return nil, err
	}
	return res.Body(), nil
}

func getRandomProxy() string {
	if ProxyFilePath == "" {
		return ""
	}
	proxies, err := os.ReadFile(ProxyFilePath)
	if err != nil {
		log.Error().Err(err).Msg("Failed to read proxy file")
		return ""
	}
	proxyList := strings.Split(string(proxies), "\n")
	if len(proxyList) == 0 {
		return ""
	}
	randIndex := rand.Intn(len(proxyList))
	return proxyList[randIndex]
}

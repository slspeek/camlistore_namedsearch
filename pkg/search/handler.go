/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/context"
	"camlistore.org/pkg/httputil"
	"camlistore.org/pkg/index"
	"camlistore.org/pkg/jsonconfig"
	"camlistore.org/pkg/jsonsign"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/types"
	"camlistore.org/pkg/types/camtypes"
)

const buffered = 32     // arbitrary channel buffer size
const maxResults = 1000 // arbitrary limit on the number of search results returned
const defaultNumResults = 50

// MaxImageSize is the maximum width or height in pixels that we will serve image
// thumbnails at. It is used in the search result UI.
const MaxImageSize = 2000

var blobRefPattern = regexp.MustCompile(blob.Pattern)

func init() {
	blobserver.RegisterHandlerConstructor("search", newHandlerFromConfig)
}

// Handler handles search queries.
type Handler struct {
	index           index.Interface
	storageAndIndex blobserver.Storage
	owner           blob.Ref

	// Corpus optionally specifies the full in-memory metadata corpus
	// to use.
	// TODO: this may be required in the future, or folded into the index
	// interface.
	corpus *index.Corpus

	// WebSocket hub
	wsHub *wsHub
}

// IGetRecentPermanodes is the interface encapsulating the GetRecentPermanodes query.
type IGetRecentPermanodes interface {
	// GetRecentPermanodes returns recently-modified permanodes.
	// This is a higher-level query returning more metadata than the index.GetRecentPermanodes,
	// which only scans the blobrefs but doesn't return anything about the permanodes.
	// TODO: rename this one?
	GetRecentPermanodes(*RecentRequest) (*RecentResponse, error)
}

var (
	_ IGetRecentPermanodes = (*Handler)(nil)
)

func NewHandler(index index.Interface, storageAndIndex blobserver.Storage, owner blob.Ref) *Handler {
	sh := &Handler{
		index:           index,
		storageAndIndex: storageAndIndex,
		owner:           owner,
	}
	sh.wsHub = newWebsocketHub(sh)
	go sh.wsHub.run()
	sh.subscribeToNewBlobs()
	replaceKeyword(newNamedSearch(sh))
	return sh
}

func (sh *Handler) subscribeToNewBlobs() {
	ch := make(chan blob.Ref, buffered)
	blobserver.GetHub(sh.index).RegisterListener(ch)
	go func() {
		for br := range ch {
			bm, err := sh.index.GetBlobMeta(br)
			if err == nil {
				sh.wsHub.newBlobRecv <- bm.CamliType
			}
		}
	}()
}

func (h *Handler) SetCorpus(c *index.Corpus) {
	h.corpus = c
}

func newHandlerFromConfig(ld blobserver.Loader, conf jsonconfig.Obj) (http.Handler, error) {
	indexPrefix := conf.RequiredString("index") // TODO: add optional help tips here?
	ownerBlobStr := conf.RequiredString("owner")
	storageAndIndexPrefix := conf.RequiredString("storageAndIndex")

	devBlockStartupPrefix := conf.OptionalString("devBlockStartupOn", "")
	slurpToMemory := conf.OptionalBool("slurpToMemory", false)
	if err := conf.Validate(); err != nil {
		return nil, err
	}

	if devBlockStartupPrefix != "" {
		_, err := ld.GetHandler(devBlockStartupPrefix)
		if err != nil {
			return nil, fmt.Errorf("search handler references bogus devBlockStartupOn handler %s: %v", devBlockStartupPrefix, err)
		}
	}

	storageAndIndexHandler, err := ld.GetStorage(storageAndIndexPrefix)
	if err != nil {
		return nil, fmt.Errorf("search config references unknown handler %q", storageAndIndexPrefix)
	}

	indexHandler, err := ld.GetHandler(indexPrefix)
	if err != nil {
		return nil, fmt.Errorf("search config references unknown handler %q", indexPrefix)
	}
	indexer, ok := indexHandler.(index.Interface)
	if !ok {
		return nil, fmt.Errorf("search config references invalid indexer %q (actually a %T)", indexPrefix, indexHandler)
	}
	ownerBlobRef, ok := blob.Parse(ownerBlobStr)
	if !ok {
		return nil, fmt.Errorf("search 'owner' has malformed blobref %q; expecting e.g. sha1-xxxxxxxxxxxx",
			ownerBlobStr)
	}
	ii := indexer.(*index.Index)
	h := NewHandler(ii, storageAndIndexHandler, ownerBlobRef)
	if slurpToMemory {
		corpus, err := ii.KeepInMemory()
		if err != nil {
			return nil, fmt.Errorf("error slurping index to memory: %v", err)
		}
		h.corpus = corpus
	}

	return h, nil
}

// Owner returns Handler owner's public key blobref.
func (h *Handler) Owner() blob.Ref {
	// TODO: figure out a plan for an owner having multiple active public keys, or public
	// key rotation
	return h.owner
}

func (h *Handler) Index() index.Interface {
	return h.index
}

func jsonMap() map[string]interface{} {
	return make(map[string]interface{})
}

var getHandler = map[string]func(*Handler, http.ResponseWriter, *http.Request){
	"ws":              (*Handler).serveWebSocket,
	"recent":          (*Handler).serveRecentPermanodes,
	"permanodeattr":   (*Handler).servePermanodesWithAttr,
	"describe":        (*Handler).serveDescribe,
	"claims":          (*Handler).serveClaims,
	"files":           (*Handler).serveFiles,
	"getnamed":        (*Handler).serveGetNamed,
	"signerattrvalue": (*Handler).serveSignerAttrValue,
	"signerpaths":     (*Handler).serveSignerPaths,
	"edgesto":         (*Handler).serveEdgesTo,
}

var postHandler = map[string]func(*Handler, http.ResponseWriter, *http.Request){
	"describe": (*Handler).serveDescribe,
	"query":    (*Handler).serveQuery,
	"setnamed": (*Handler).serveSetNamed,
}

func (sh *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	suffix := httputil.PathSuffix(req)

	handlers := getHandler
	switch {
	case httputil.IsGet(req):
		// use default from above
	case req.Method == "POST":
		handlers = postHandler
	default:
		handlers = nil
	}
	fn := handlers[strings.TrimPrefix(suffix, "camli/search/")]
	if fn != nil {
		fn(sh, rw, req)
		return
	}

	// TODO: discovery for the endpoints & better error message with link to discovery info
	ret := jsonMap()
	ret["error"] = "Unsupported search path or method"
	ret["errorType"] = "input"
	httputil.ReturnJSON(rw, ret)
}

// sanitizeNumResults takes n as a requested number of search results and sanitizes it.
func sanitizeNumResults(n int) int {
	if n <= 0 || n > maxResults {
		return defaultNumResults
	}
	return n
}

// RecentRequest is a request to get a RecentResponse.
type RecentRequest struct {
	N             int       // if zero, default number of results
	Before        time.Time // if zero, now
	ThumbnailSize int       // if zero, no thumbnails
}

func (r *RecentRequest) URLSuffix() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "camli/search/recent?n=%d&thumbnails=%d", r.n(), r.thumbnailSize())
	if !r.Before.IsZero() {
		fmt.Fprintf(&buf, "&before=%s", types.Time3339(r.Before))
	}
	return buf.String()
}

// fromHTTP panics with an httputil value on failure
func (r *RecentRequest) fromHTTP(req *http.Request) {
	r.N, _ = strconv.Atoi(req.FormValue("n"))
	r.ThumbnailSize = thumbnailSize(req)
	if before := req.FormValue("before"); before != "" {
		r.Before = time.Time(types.ParseTime3339OrZero(before))
	}
}

// n returns the sanitized maximum number of search results.
func (r *RecentRequest) n() int {
	return sanitizeNumResults(r.N)
}

func (r *RecentRequest) thumbnailSize() int {
	v := r.ThumbnailSize
	if v == 0 {
		return 0
	}
	if v < minThumbSize || v > maxThumbSize {
		return defThumbSize
	}
	return v
}

// WithAttrRequest is a request to get a WithAttrResponse.
type WithAttrRequest struct {
	N      int      // max number of results
	Signer blob.Ref // if nil, will use the server's default owner (if configured)
	// Requested attribute. If blank, all attributes are searched (for Value)
	// as fulltext.
	Attr string
	// Value of the requested attribute. If blank, permanodes which have
	// request.Attr as an attribute are searched.
	Value         string
	Fuzzy         bool // fulltext search (if supported).
	ThumbnailSize int  // if zero, no thumbnails
}

func (r *WithAttrRequest) URLSuffix() string {
	return fmt.Sprintf("camli/search/permanodeattr?signer=%v&value=%v&fuzzy=%v&attr=%v&max=%v&thumbnails=%v",
		r.Signer, url.QueryEscape(r.Value), r.Fuzzy, r.Attr, r.N, r.ThumbnailSize)
}

// fromHTTP panics with an httputil value on failure
func (r *WithAttrRequest) fromHTTP(req *http.Request) {
	r.Signer = blob.ParseOrZero(req.FormValue("signer"))
	r.Value = req.FormValue("value")
	fuzzy := req.FormValue("fuzzy") // exact match if empty
	fuzzyMatch := false
	if fuzzy != "" {
		lowered := strings.ToLower(fuzzy)
		if lowered == "true" || lowered == "t" {
			fuzzyMatch = true
		}
	}
	r.Attr = req.FormValue("attr") // all attributes if empty
	if r.Attr == "" {              // and force fuzzy in that case.
		fuzzyMatch = true
	}
	r.Fuzzy = fuzzyMatch
	r.ThumbnailSize = thumbnailSize(req)
	max := req.FormValue("max")
	if max != "" {
		maxR, err := strconv.Atoi(max)
		if err != nil {
			panic(httputil.InvalidParameterError("max"))
		}
		r.N = maxR
	}
	r.N = r.n()
}

// n returns the sanitized maximum number of search results.
func (r *WithAttrRequest) n() int {
	return sanitizeNumResults(r.N)
}

func (r *WithAttrRequest) thumbnailSize() int {
	v := r.ThumbnailSize
	if v == 0 {
		return 0
	}
	if v < minThumbSize {
		return minThumbSize
	}
	if v > maxThumbSize {
		return maxThumbSize
	}
	return v
}

// ClaimsRequest is a request to get a ClaimsResponse.
type ClaimsRequest struct {
	Permanode blob.Ref

	// AttrFilter optionally filters claims about the given attribute.
	// If empty, all claims for the given Permanode are returned.
	AttrFilter string
}

func (r *ClaimsRequest) URLSuffix() string {
	return fmt.Sprintf("camli/search/claims?permanode=%v&attrFilter=%s",
		r.Permanode, url.QueryEscape(r.AttrFilter))
}

// fromHTTP panics with an httputil value on failure
func (r *ClaimsRequest) fromHTTP(req *http.Request) {
	r.Permanode = httputil.MustGetBlobRef(req, "permanode")
	r.AttrFilter = req.FormValue("attrFilter")
}

// SignerPathsRequest is a request to get a SignerPathsResponse.
type SignerPathsRequest struct {
	Signer blob.Ref
	Target blob.Ref
}

// fromHTTP panics with an httputil value on failure
func (r *SignerPathsRequest) fromHTTP(req *http.Request) {
	r.Signer = httputil.MustGetBlobRef(req, "signer")
	r.Target = httputil.MustGetBlobRef(req, "target")
}

// EdgesRequest is a request to get an EdgesResponse.
type EdgesRequest struct {
	// The blob we want to find as a reference.
	ToRef blob.Ref
}

// fromHTTP panics with an httputil value on failure
func (r *EdgesRequest) fromHTTP(req *http.Request) {
	r.ToRef = httputil.MustGetBlobRef(req, "blobref")
}

// TODO(mpl): it looks like we never populate RecentResponse.Error*, shouldn't we remove them?
// Same for WithAttrResponse. I suppose it doesn't matter much if we end up removing GetRecentPermanodes anyway...

// GetNamedRequest is a request to get the substitute for a named:foo expression
type GetNamedRequest struct {
	Named string
}

func (sr *GetNamedRequest) fromHTTP(req *http.Request) {
	sr.Named = req.FormValue("named")
}

// SetNamedRequest is a request to set the substitute for a named:foo expression
type SetNamedRequest struct {
	Named      string
	Substitute string
}

func (sr *SetNamedRequest) fromHTTP(req *http.Request) {
	sr.Named = req.FormValue("named")
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		panic(err)
	}
	sr.Substitute = string(body)
}

// RecentResponse is the JSON response from $searchRoot/camli/search/recent.
type RecentResponse struct {
	Recent []*RecentItem `json:"recent"`
	Meta   MetaMap       `json:"meta"`

	Error     string `json:"error,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
}

func (r *RecentResponse) Err() error {
	if r.Error != "" || r.ErrorType != "" {
		if r.ErrorType != "" {
			return fmt.Errorf("%s: %s", r.ErrorType, r.Error)
		}
		return errors.New(r.Error)
	}
	return nil
}

// WithAttrResponse is the JSON response from $searchRoot/camli/search/permanodeattr.
type WithAttrResponse struct {
	WithAttr []*WithAttrItem `json:"withAttr"`
	Meta     MetaMap         `json:"meta"`

	Error     string `json:"error,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
}

func (r *WithAttrResponse) Err() error {
	if r.Error != "" || r.ErrorType != "" {
		if r.ErrorType != "" {
			return fmt.Errorf("%s: %s", r.ErrorType, r.Error)
		}
		return errors.New(r.Error)
	}
	return nil
}

// ClaimsResponse is the JSON response from $searchRoot/camli/search/claims.
type ClaimsResponse struct {
	Claims []*ClaimsItem `json:"claims"`
}

// SignerPathsResponse is the JSON response from $searchRoot/camli/search/signerpaths.
type SignerPathsResponse struct {
	Paths []*SignerPathsItem `json:"paths"`
	Meta  MetaMap            `json:"meta"`
}

// A RecentItem is an item returned from $searchRoot/camli/search/recent in the "recent" list.
type RecentItem struct {
	BlobRef blob.Ref       `json:"blobref"`
	ModTime types.Time3339 `json:"modtime"`
	Owner   blob.Ref       `json:"owner"`
}

// A WithAttrItem is an item returned from $searchRoot/camli/search/permanodeattr.
type WithAttrItem struct {
	Permanode blob.Ref `json:"permanode"`
}

// A ClaimsItem is an item returned from $searchRoot/camli/search/claims.
type ClaimsItem struct {
	BlobRef   blob.Ref       `json:"blobref"`
	Signer    blob.Ref       `json:"signer"`
	Permanode blob.Ref       `json:"permanode"`
	Date      types.Time3339 `json:"date"`
	Type      string         `json:"type"`
	Attr      string         `json:"attr,omitempty"`
	Value     string         `json:"value,omitempty"`
}

// A SignerPathsItem is an item returned from $searchRoot/camli/search/signerpaths.
type SignerPathsItem struct {
	ClaimRef blob.Ref `json:"claimRef"`
	BaseRef  blob.Ref `json:"baseRef"`
	Suffix   string   `json:"suffix"`
}

// EdgesResponse is the JSON response from $searchRoot/camli/search/edgesto.
type EdgesResponse struct {
	ToRef   blob.Ref    `json:"toRef"`
	EdgesTo []*EdgeItem `json:"edgesTo"`
}

// An EdgeItem is an item returned from $searchRoot/camli/search/edgesto.
type EdgeItem struct {
	From     blob.Ref `json:"from"`
	FromType string   `json:"fromType"`
}

// GetNamedResponse is the JSON response from $searchRoot/camli/search/getnamed.
type GetNamedResponse struct {
	Named      string   `json:"named"`
	Substitute string   `json:"substitute"`
	PermaRef   blob.Ref `json:"permaRef"`
	SubstRef   blob.Ref `json:"substRef"`
}

// SetNamedResponse is the JSON response from $searchRoot/camli/search/setnamed.
type SetNamedResponse struct {
	PermaRef blob.Ref `json:"permaRef"`
	SubstRef blob.Ref `json:"substRef"`
}

func thumbnailSize(r *http.Request) int {
	return thumbnailSizeStr(r.FormValue("thumbnails"))
}

const (
	minThumbSize = 25
	defThumbSize = 50
	maxThumbSize = 800
)

func thumbnailSizeStr(s string) int {
	if s == "" {
		return 0
	}
	if i, _ := strconv.Atoi(s); i >= minThumbSize && i <= maxThumbSize {
		return i
	}
	return defThumbSize
}

var testHookBug121 = func() {}

// GetRecentPermanodes returns recently-modified permanodes.
func (sh *Handler) GetRecentPermanodes(req *RecentRequest) (*RecentResponse, error) {
	ch := make(chan camtypes.RecentPermanode)
	errch := make(chan error, 1)
	before := time.Now()
	if !req.Before.IsZero() {
		before = req.Before
	}
	go func() {
		errch <- sh.index.GetRecentPermanodes(ch, sh.owner, req.n(), before)
	}()

	dr := sh.NewDescribeRequest()

	var recent []*RecentItem
	for res := range ch {
		dr.Describe(res.Permanode, 2)
		recent = append(recent, &RecentItem{
			BlobRef: res.Permanode,
			Owner:   res.Signer,
			ModTime: types.Time3339(res.LastModTime),
		})
		testHookBug121() // http://camlistore.org/issue/121
	}

	if err := <-errch; err != nil {
		return nil, err
	}

	metaMap, err := dr.metaMapThumbs(req.thumbnailSize())
	if err != nil {
		return nil, err
	}

	res := &RecentResponse{
		Recent: recent,
		Meta:   metaMap,
	}
	return res, nil
}

func (sh *Handler) serveRecentPermanodes(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var rr RecentRequest
	rr.fromHTTP(req)
	res, err := sh.GetRecentPermanodes(&rr)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

// GetPermanodesWithAttr returns permanodes with attribute req.Attr
// having the req.Value as a value.
// See WithAttrRequest for more details about the query.
func (sh *Handler) GetPermanodesWithAttr(req *WithAttrRequest) (*WithAttrResponse, error) {
	ch := make(chan blob.Ref, buffered)
	errch := make(chan error, 1)
	go func() {
		signer := req.Signer
		if !signer.Valid() {
			signer = sh.owner
		}
		errch <- sh.index.SearchPermanodesWithAttr(ch,
			&camtypes.PermanodeByAttrRequest{
				Attribute:  req.Attr,
				Query:      req.Value,
				Signer:     signer,
				FuzzyMatch: req.Fuzzy,
				MaxResults: req.N,
			})
	}()

	dr := sh.NewDescribeRequest()

	var withAttr []*WithAttrItem
	for res := range ch {
		dr.Describe(res, 2)
		withAttr = append(withAttr, &WithAttrItem{
			Permanode: res,
		})
	}

	metaMap, err := dr.metaMapThumbs(req.thumbnailSize())
	if err != nil {
		return nil, err
	}

	if err := <-errch; err != nil {
		return nil, err
	}

	res := &WithAttrResponse{
		WithAttr: withAttr,
		Meta:     metaMap,
	}
	return res, nil
}

// servePermanodesWithAttr uses the indexer to search for the permanodes matching
// the request.
// The valid values for the "attr" key in the request (i.e the only attributes
// for a permanode which are actually indexed as such) are "tag" and "title".
func (sh *Handler) servePermanodesWithAttr(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var wr WithAttrRequest
	wr.fromHTTP(req)
	res, err := sh.GetPermanodesWithAttr(&wr)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

// GetClaims returns the claims on req.Permanode signed by sh.owner.
func (sh *Handler) GetClaims(req *ClaimsRequest) (*ClaimsResponse, error) {
	if !req.Permanode.Valid() {
		return nil, errors.New("Error getting claims: nil permanode.")
	}
	var claims []camtypes.Claim
	claims, err := sh.index.AppendClaims(claims, req.Permanode, sh.owner, req.AttrFilter)
	if err != nil {
		return nil, fmt.Errorf("Error getting claims of %s: %v", req.Permanode.String(), err)
	}
	sort.Sort(camtypes.ClaimsByDate(claims))
	var jclaims []*ClaimsItem
	for _, claim := range claims {
		jclaim := &ClaimsItem{
			BlobRef:   claim.BlobRef,
			Signer:    claim.Signer,
			Permanode: claim.Permanode,
			Date:      types.Time3339(claim.Date),
			Type:      claim.Type,
			Attr:      claim.Attr,
			Value:     claim.Value,
		}
		jclaims = append(jclaims, jclaim)
	}

	res := &ClaimsResponse{
		Claims: jclaims,
	}
	return res, nil
}

func (sh *Handler) serveClaims(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var cr ClaimsRequest
	cr.fromHTTP(req)
	res, err := sh.GetClaims(&cr)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

func (sh *Handler) serveFiles(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJSON(rw, ret)

	br, ok := blob.Parse(req.FormValue("wholedigest"))
	if !ok {
		ret["error"] = "Missing or invalid 'wholedigest' param"
		ret["errorType"] = "input"
		return
	}

	files, err := sh.index.ExistingFileSchemas(br)
	if err != nil {
		ret["error"] = err.Error()
		ret["errorType"] = "server"
		return
	}

	strList := []string{}
	for _, br := range files {
		strList = append(strList, br.String())
	}
	ret["files"] = strList
	return
}

// SignerAttrValueResponse is the JSON response to $search/camli/search/signerattrvalue
type SignerAttrValueResponse struct {
	Permanode blob.Ref `json:"permanode"`
	Meta      MetaMap  `json:"meta"`
}

func (sh *Handler) serveSignerAttrValue(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	signer := httputil.MustGetBlobRef(req, "signer")
	attr := httputil.MustGet(req, "attr")
	value := httputil.MustGet(req, "value")

	pn, err := sh.index.PermanodeOfSignerAttrValue(signer, attr, value)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}

	dr := sh.NewDescribeRequest()
	dr.Describe(pn, 2)
	metaMap, err := dr.metaMap()
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}

	httputil.ReturnJSON(rw, &SignerAttrValueResponse{
		Permanode: pn,
		Meta:      metaMap,
	})
}

// EdgesTo returns edges that reference req.RefTo.
// It filters out since-deleted permanode edges.
func (sh *Handler) EdgesTo(req *EdgesRequest) (*EdgesResponse, error) {
	toRef := req.ToRef
	toRefStr := toRef.String()
	var edgeItems []*EdgeItem

	edges, err := sh.index.EdgesTo(toRef, nil)
	if err != nil {
		panic(err)
	}

	type edgeOrError struct {
		edge *EdgeItem // or nil
		err  error
	}
	resc := make(chan edgeOrError)
	verify := func(edge *camtypes.Edge) {
		db, err := sh.NewDescribeRequest().DescribeSync(edge.From)
		if err != nil {
			resc <- edgeOrError{err: err}
			return
		}
		found := false
		if db.Permanode != nil {
			for attr, vv := range db.Permanode.Attr {
				if index.IsBlobReferenceAttribute(attr) {
					for _, v := range vv {
						if v == toRefStr {
							found = true
						}
					}
				}
			}
		}
		var ei *EdgeItem
		if found {
			ei = &EdgeItem{
				From:     edge.From,
				FromType: "permanode",
			}
		}
		resc <- edgeOrError{edge: ei}
	}
	verifying := 0
	for _, edge := range edges {
		if edge.FromType == "permanode" {
			verifying++
			go verify(edge)
			continue
		}
		ei := &EdgeItem{
			From:     edge.From,
			FromType: edge.FromType,
		}
		edgeItems = append(edgeItems, ei)
	}
	for i := 0; i < verifying; i++ {
		res := <-resc
		if res.err != nil {
			return nil, res.err
		}
		if res.edge != nil {
			edgeItems = append(edgeItems, res.edge)
		}
	}

	return &EdgesResponse{
		ToRef:   toRef,
		EdgesTo: edgeItems,
	}, nil
}

// Unlike the index interface's EdgesTo method, the "edgesto" Handler
// here additionally filters out since-deleted permanode edges.
func (sh *Handler) serveEdgesTo(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var er EdgesRequest
	er.fromHTTP(req)
	res, err := sh.EdgesTo(&er)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

func (sh *Handler) serveQuery(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)

	var sq SearchQuery
	if err := sq.fromHTTP(req); err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}

	sr, err := sh.Query(&sq)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}

	httputil.ReturnJSON(rw, sr)
}

// GetSignerPaths returns paths with a target of req.Target.
func (sh *Handler) GetSignerPaths(req *SignerPathsRequest) (*SignerPathsResponse, error) {
	if !req.Signer.Valid() {
		return nil, errors.New("Error getting signer paths: nil signer.")
	}
	if !req.Target.Valid() {
		return nil, errors.New("Error getting signer paths: nil target.")
	}
	paths, err := sh.index.PathsOfSignerTarget(req.Signer, req.Target)
	if err != nil {
		return nil, fmt.Errorf("Error getting paths of %s: %v", req.Target.String(), err)
	}
	var jpaths []*SignerPathsItem
	for _, path := range paths {
		jpaths = append(jpaths, &SignerPathsItem{
			ClaimRef: path.Claim,
			BaseRef:  path.Base,
			Suffix:   path.Suffix,
		})
	}

	dr := sh.NewDescribeRequest()
	for _, path := range paths {
		dr.Describe(path.Base, 2)
	}
	metaMap, err := dr.metaMap()
	if err != nil {
		return nil, err
	}

	res := &SignerPathsResponse{
		Paths: jpaths,
		Meta:  metaMap,
	}
	return res, nil
}

func (sh *Handler) serveSignerPaths(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var sr SignerPathsRequest
	sr.fromHTTP(req)

	res, err := sh.GetSignerPaths(&sr)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

func evalSearchInput(in string) (*Constraint, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	if strings.HasPrefix(in, "{") && strings.HasSuffix(in, "}") {
		cs := new(Constraint)
		if err := json.NewDecoder(strings.NewReader(in)).Decode(&cs); err != nil {
			return nil, err
		}
		return cs, nil
	} else {
		sq, err := parseExpression(context.TODO(), in)
		if err != nil {
			return nil, err
		}
		return sq.Constraint.Logical.B, nil
	}
}

// SetNamed creates or modifies a search expression alias.
func (sh *Handler) SetNamed(r *SetNamedRequest) (*SetNamedResponse, error) {
	if _, err := evalSearchInput(r.Substitute); err != nil {
		return nil, err
	}
	ssref, err := sh.receiveString(r.Substitute)
	if err != nil {
		return nil, err
	}
	sref := ssref.Ref

	var pn blob.Ref
	claims := []*schema.Builder{}
	gr, err := sh.GetNamed(&GetNamedRequest{Named: r.Named})
	if err == nil {
		pn = gr.PermaRef
	} else {
		pnSRef, err := sh.receiveAndSign(schema.NewUnsignedPermanode())
		if err != nil {
			return nil, err
		}
		pn = pnSRef.Ref
		claims = append(claims, schema.NewSetAttributeClaim(pn, "camliNamedSearch", r.Named))
		claims = append(claims, schema.NewSetAttributeClaim(pn, "title", fmt.Sprintf("named:%s", r.Named)))
	}
	claims = append(claims, schema.NewSetAttributeClaim(pn, "camliContent", sref.String()))
	for _, claimBuilder := range claims {
		_, err := sh.receiveAndSign(claimBuilder)
		if err != nil {
			return nil, err
		}
	}
	return &SetNamedResponse{PermaRef: pn, SubstRef: sref}, nil
}

func (sh *Handler) receiveAndSign(b *schema.Builder) (*blob.SizedRef, error) {
	b.SetSigner(sh.owner)
	unsigned, err := b.JSON()
	if err != nil {
		return nil, err
	}
	sr := &jsonsign.SignRequest{
		UnsignedJSON:  unsigned,
		Fetcher:       sh.storageAndIndex,
		SignatureTime: time.Now(),
	}
	signed, err := sr.Sign()
	if err != nil {
		return nil, err
	}
	sref, err := sh.receiveString(signed)
	if err != nil {
		return nil, err
	}
	return sref, nil
}

func (sh *Handler) receiveString(s string) (*blob.SizedRef, error) {
	sref, err := blobserver.ReceiveString(sh.storageAndIndex, s)
	if err != nil {
		return nil, err
	}
	return &sref, nil
}

func (sh *Handler) serveSetNamed(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	r := new(SetNamedRequest)
	r.fromHTTP(req)

	res, err := sh.SetNamed(r)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

// GetNamed displays the search expression or constraint json for the requested alias.
func (sh *Handler) GetNamed(r *GetNamedRequest) (*GetNamedResponse, error) {
	sr, err := sh.Query(&SearchQuery{
		Constraint: &Constraint{
			Permanode: &PermanodeConstraint{
				Attr:  "camliNamedSearch",
				Value: r.Named,
			},
		},
		Describe: &DescribeRequest{},
	})
	if err != nil {
		return nil, err
	}

	if len(sr.Blobs) < 1 {
		return nil, fmt.Errorf("No named search found for: %s", r.Named)
	}
	res := new(GetNamedResponse)
	res.Named = r.Named
	res.PermaRef = sr.Blobs[0].Blob
	substRefS := sr.Describe.Meta.Get(res.PermaRef).Permanode.Attr.Get("camliContent")
	br, ok := blob.Parse(substRefS)
	if !ok {
		return nil, fmt.Errorf("Invalid blob ref: %s", substRefS)
	}

	reader, _, err := sh.storageAndIndex.Fetch(br)
	if err != nil {
		return nil, err
	}
	result, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	res.Substitute = string(result)
	res.SubstRef = br
	return res, nil
}

func (sh *Handler) serveGetNamed(rw http.ResponseWriter, req *http.Request) {
	defer httputil.RecoverJSON(rw, req)
	var sr GetNamedRequest
	sr.fromHTTP(req)

	res, err := sh.GetNamed(&sr)
	if err != nil {
		httputil.ServeJSONError(rw, err)
		return
	}
	httputil.ReturnJSON(rw, res)
}

const camliTypePrefix = "application/json; camliType="

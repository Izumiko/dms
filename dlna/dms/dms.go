package dms

import (
	"bitbucket.org/anacrolix/dms/dlna"
	"bitbucket.org/anacrolix/dms/ffmpeg"
	"bitbucket.org/anacrolix/dms/futures"
	"bitbucket.org/anacrolix/dms/misc"
	"bitbucket.org/anacrolix/dms/soap"
	"bitbucket.org/anacrolix/dms/ssdp"
	"bitbucket.org/anacrolix/dms/upnp"
	"bitbucket.org/anacrolix/dms/upnpav"
	"bytes"
	"crypto/md5"
	"encoding/ascii85"
	"encoding/gob"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	serverField                 = "Linux/3.4 DLNADOC/1.50 UPnP/1.0 DMS/1.0"
	rootDeviceType              = "urn:schemas-upnp-org:device:MediaServer:1"
	rootDeviceModelName         = "dms 1.0"
	resPath                     = "/res"
	rootDescPath                = "/rootDesc.xml"
	contentDirectorySCPDURL     = "/scpd/ContentDirectory.xml"
	contentDirectoryEventSubURL = "/evt/ContentDirectory"
	contentDirectoryControlURL  = "/ctl/ContentDirectory"
	transcodeMimeType           = "video/mpeg"
)

func makeDeviceUuid(unique string) string {
	h := md5.New()
	if _, err := io.WriteString(h, unique); err != nil {
		panic(err)
	}
	buf := h.Sum(nil)
	return fmt.Sprintf("uuid:%x-%x-%x-%x-%x", buf[:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

var services = []upnp.Service{
	upnp.Service{
		ServiceType: "urn:schemas-upnp-org:service:ContentDirectory:1",
		ServiceId:   "urn:upnp-org:serviceId:ContentDirectory",
		SCPDURL:     contentDirectorySCPDURL,
		ControlURL:  contentDirectoryControlURL,
		EventSubURL: contentDirectoryEventSubURL,
	},
}

func devices() []string {
	return []string{
		"urn:schemas-upnp-org:device:MediaServer:1",
	}
}

func serviceTypes() (ret []string) {
	for _, s := range services {
		ret = append(ret, s.ServiceType)
	}
	return
}
func (me *Server) httpPort() int {
	return me.HTTPConn.Addr().(*net.TCPAddr).Port
}

func (me *Server) serveHTTP() {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Ext", "")
			w.Header().Set("Server", serverField)
			me.httpServeMux.ServeHTTP(w, r)
		}),
	}
	err := srv.Serve(me.HTTPConn)
	log.Fatalln(err)
}

func (me *Server) doSSDP() {
	active := 0
	stopped := make(chan struct{})
	for _, if_ := range me.Interfaces {
		s := ssdp.Server{
			Interface: if_,
			Devices:   devices(),
			Services:  serviceTypes(),
			Location: func(ip net.IP) string {
				return me.location(ip)
			},
			Server:         serverField,
			UUID:           me.rootDeviceUUID,
			NotifyInterval: 30,
		}
		active++
		go func(if_ net.Interface) {
			if err := s.Init(); err != nil {
				log.Println(if_.Name, err)
			} else {
				log.Println("started SSDP on", if_.Name)
				if err := s.Serve(); err != nil {
					log.Printf("%q: %q\n", if_.Name, err)
				}
				s.Close()
			}
			stopped <- struct{}{}
		}(if_)
	}
	for active > 0 {
		<-stopped
		active--
	}
	log.Fatalln("no interfaces remain")
}

var (
	startTime time.Time
)

type Server struct {
	HTTPConn       net.Listener
	FriendlyName   string
	Interfaces     []net.Interface
	httpServeMux   *http.ServeMux
	RootObjectPath string
	rootDescXML    []byte
	rootDeviceUUID string
}

func (me *Server) childCount(path_ string) int {
	f, err := os.Open(path_)
	if err != nil {
		log.Println(err)
		return 0
	}
	defer f.Close()
	fis, err := f.Readdir(-1)
	if err != nil {
		log.Println(err)
		return 0
	}
	ret := 0
	for _, fi := range fis {
		ret += len(me.fileEntries(fi, path_))
	}
	return ret
}

// update the UPnP object fields from ffprobe data
// priority is given the format section, and then the streams sequentially
func itemExtra(item *upnpav.Object, info *ffmpeg.Info) {
	setFromTags := func(m map[string]string) {
		for key, val := range m {
			setIfUnset := func(s *string) {
				if *s == "" {
					*s = val
				}
			}
			switch strings.ToLower(key) {
			case "tag:artist":
				setIfUnset(&item.Artist)
			case "tag:album":
				setIfUnset(&item.Album)
			case "tag:genre":
				setIfUnset(&item.Genre)
			}
		}
	}
	setFromTags(info.Format)
	for _, m := range info.Streams {
		setFromTags(m)
	}
}

// returns res attributes for the raw stream
func (me *Server) itemResExtra(info *ffmpeg.Info) (bitrate uint, duration string) {
	fmt.Sscan(info.Format["bit_rate"], &bitrate)
	if d := info.Format["duration"]; d != "" && d != "N/A" {
		var f float64
		_, err := fmt.Sscan(info.Format["duration"], &f)
		if err != nil {
			log.Println(err)
		} else {
			duration = misc.FormatDurationSexagesimal(time.Duration(f * float64(time.Second)))
		}
	}
	return
}

// Used to determine the MIME-type for the given path
func MimeTypeByPath(path_ string) (ret string) {
	ext := strings.ToLower(path.Ext(path_))
	ret = mime.TypeByExtension(ext)
	if ret != "" {
		return
	}
	file, _ := os.Open(path_)
	if file == nil {
		return
	}
	var data [512]byte
	n, _ := file.Read(data[:])
	file.Close()
	ret = http.DetectContentType(data[:n])
	return
}

func (me *Server) entryObject(entry cdsEntry, host string) interface{} {
	obj := upnpav.Object{
		ID: objectID{
			Path: entry.Path,
		}.Encode(me.RootObjectPath),
		Restricted: 1,
		ParentID:   entry.ParentID,
	}
	if entry.FileInfo.IsDir() {
		obj.Class = "object.container.storageFolder"
		obj.Title = entry.FileInfo.Name()
		return upnpav.Container{
			Object:     obj,
			ChildCount: me.childCount(entry.Path),
		}
	}
	mimeType := MimeTypeByPath(entry.Path)
	mimeTypeType := strings.SplitN(mimeType, "/", 2)[0]
	obj.Class = "object.item." + mimeTypeType + "Item"
	ffInfo, err := ffmpeg.Probe(entry.Path)
	err = suppressFFmpegProbeDataErrors(err)
	if err != nil {
		log.Printf("error probing %s: %s", entry.Path, err)
	}
	nativeBitrate, duration := me.itemResExtra(ffInfo)
	if obj.Title == "" {
		obj.Title = entry.FileInfo.Name()
	}
	return upnpav.Item{
		Object: obj,
		Res: func() (ret []upnpav.Resource) {
			ret = append(ret, func() upnpav.Resource {
				return upnpav.Resource{
					URL: (&url.URL{
						Scheme: "http",
						Host:   host,
						Path:   resPath,
						RawQuery: url.Values{
							"path": {entry.Path},
						}.Encode(),
					}).String(),
					ProtocolInfo: fmt.Sprintf("http-get:*:%s:%s", mimeType, dlna.ContentFeatures{
						SupportRange: true,
					}.String()),
					Bitrate:  nativeBitrate,
					Duration: duration,
					Size:     uint64(entry.FileInfo.Size()),
					Resolution: func() string {
						for _, strm := range ffInfo.Streams {
							if strm["codec_type"] != "video" {
								continue
							}
							width := strm["width"]
							height := strm["height"]
							if width != "" && height != "" {
								return fmt.Sprintf("%sx%s", width, height)
							}
						}
						return ""
					}(),
				}
			}())
			if mimeTypeType == "video" {
				ret = append(ret, upnpav.Resource{
					ProtocolInfo: fmt.Sprintf("http-get:*:%s:%s", transcodeMimeType, dlna.ContentFeatures{
						SupportTimeSeek: true,
						Transcoded:      true,
						ProfileName:     "MPEG_PS_PAL",
					}.String()),
					URL: (&url.URL{
						Scheme: "http",
						Host:   host,
						Path:   resPath,
						RawQuery: url.Values{
							"path":      {entry.Path},
							"transcode": {"t"},
						}.Encode(),
					}).String(),
					Resolution: "720x576",
				},
				)
			}
			return
		}(),
	}
}

func (server *Server) fileEntries(fileInfo os.FileInfo, parentPath string) []cdsEntry {
	return []cdsEntry{{
		FileInfo: fileInfo,
		Path:     path.Join(parentPath, fileInfo.Name()),
		ParentID: objectID{
			Path: parentPath,
		}.Encode(server.RootObjectPath),
	}}
}

// Sufficient information to determine how many entries to each actual file
type cdsEntry struct {
	FileInfo os.FileInfo // file type. names would do but it's cheaper to do this upfront.
	Path     string
	ParentID string
}

type fileInfoSlice []os.FileInfo

func (me fileInfoSlice) Len() int {
	return len(me)
}

func (me fileInfoSlice) Less(i, j int) bool {
	return strings.ToLower(me[i].Name()) < strings.ToLower(me[j].Name())
}

func (me fileInfoSlice) Swap(i, j int) {
	me[i], me[j] = me[j], me[i]
}

func (me *Server) readContainer(path_, parentID, host string) (ret []interface{}) {
	dir, err := os.Open(path_)
	if err != nil {
		log.Println(err)
		return
	}
	defer dir.Close()
	var fis fileInfoSlice
	fis, err = dir.Readdir(-1)
	if err != nil {
		panic(err)
	}
	sort.Sort(fis)
	pool := futures.NewExecutor(runtime.NumCPU())
	defer pool.Shutdown()
	for obj := range pool.Map(func(entry interface{}) interface{} {
		return me.entryObject(entry.(cdsEntry), host)
	}, func() <-chan interface{} {
		ret := make(chan interface{})
		go func() {
			for _, fi := range fis {
				for _, entry := range me.fileEntries(fi, path_) {
					ret <- entry
				}
			}
			close(ret)
		}()
		return ret
	}()) {
		ret = append(ret, obj)
	}
	return
}

type browse struct {
	ObjectID       string
	BrowseFlag     string
	Filter         string
	StartingIndex  int
	RequestedCount int
}

type objectID struct {
	Path      string
	Transcode bool
}

func (me objectID) Encode(rootPath string) string {
	switch me.Path {
	case rootPath:
		return "0"
	case path.Dir(rootPath):
		return "-1"
	}
	w := &bytes.Buffer{}
	enc := gob.NewEncoder(w)
	if err := enc.Encode(me); err != nil {
		panic(err)
	}
	dst := make([]byte, ascii85.MaxEncodedLen(w.Len()))
	n := ascii85.Encode(dst, w.Bytes())
	return "1" + string(dst[:n])
}

func (me *Server) parseObjectID(id string) (ret objectID) {
	if id == "0" {
		ret.Path = me.RootObjectPath
		return
	}
	if id[0] != '1' {
		panic(id)
	}
	id = id[1:]
	dst := make([]byte, len(id))
	ndst, nsrc, err := ascii85.Decode(dst, []byte(id), true)
	if err != nil {
		panic(err)
	}
	if nsrc != len(id) {
		panic(nsrc)
	}
	dec := gob.NewDecoder(bytes.NewReader(dst[:ndst]))
	if err := dec.Decode(&ret); err != nil {
		panic(err)
	}
	return
}

func (me *Server) contentDirectoryResponseArgs(sa upnp.SoapAction, argsXML []byte, host string) (map[string]string, *upnp.Error) {
	switch sa.Action {
	case "GetSortCapabilities":
		return map[string]string{
			"SortCaps": "dc:title",
		}, nil
	case "Browse":
		var browse browse
		if err := xml.Unmarshal([]byte(argsXML), &browse); err != nil {
			panic(err)
		}
		objectID := me.parseObjectID(browse.ObjectID)
		switch browse.BrowseFlag {
		case "BrowseDirectChildren":
			objs := me.readContainer(objectID.Path, browse.ObjectID, host)
			totalMatches := len(objs)
			objs = objs[browse.StartingIndex:]
			if browse.RequestedCount != 0 && int(browse.RequestedCount) < len(objs) {
				objs = objs[:browse.RequestedCount]
			}
			result, err := xml.MarshalIndent(objs, "", "  ")
			if err != nil {
				panic(err)
			}
			return map[string]string{
				"TotalMatches":   fmt.Sprint(totalMatches),
				"NumberReturned": fmt.Sprint(len(objs)),
				"Result":         didl_lite(string(result)),
				"UpdateID":       fmt.Sprintf("%d", uint32(time.Now().Unix())),
			}, nil
		case "BrowseMetadata":
			fileInfo, err := os.Stat(objectID.Path)
			if err != nil {
				return nil, &upnp.Error{
					Code: upnpav.NoSuchObjectErrorCode,
					Desc: err.Error(),
				}
			}
			return map[string]string{
				"TotalMatches":   "1",
				"NumberReturned": "1",
				"Result": didl_lite(func() string {
					buf, err := xml.MarshalIndent(me.entryObject(cdsEntry{
						ParentID: browse.ObjectID,
						FileInfo: fileInfo,
						Path:     objectID.Path,
					}, host), "", "  ")
					if err != nil {
						panic(err) // because aliens
					}
					return string(buf)
				}()),
				"UpdateID": fmt.Sprintf("%d", uint32(time.Now().Unix())),
			}, nil
		default:
			return nil, &upnp.Error{
				Code: upnp.ArgumentValueInvalidErrorCode,
				Desc: fmt.Sprint("unhandled browse flag:", browse.BrowseFlag),
			}
		}
	case "GetSearchCapabilities":
		return map[string]string{
			"SearchCaps": "",
		}, nil
	}
	log.Println("unhandled content directory action:", sa.Action)
	return nil, &upnp.InvalidActionError
}

func (me *Server) serveDLNATranscode(w http.ResponseWriter, r *http.Request, path_ string) {
	w.Header().Set(dlna.TransferModeDomain, "Streaming")
	w.Header().Set("content-type", "video/mpeg")
	w.Header().Set(dlna.ContentFeaturesDomain, (dlna.ContentFeatures{
		Transcoded:      true,
		SupportTimeSeek: true,
	}).String())
	dlnaRangeHeader := r.Header.Get(dlna.TimeSeekRangeDomain)
	var nptStart, nptLength string
	if dlnaRangeHeader != "" {
		if !strings.HasPrefix(dlnaRangeHeader, "npt=") {
			log.Println("bad range:", dlnaRangeHeader)
			return
		}
		dlnaRange, err := dlna.ParseNPTRange(dlnaRangeHeader[len("npt="):])
		if err != nil {
			log.Println("bad range:", dlnaRangeHeader)
			return
		}
		nptStart = dlna.FormatNPTTime(dlnaRange.Start)
		if dlnaRange.End > 0 {
			nptLength = dlna.FormatNPTTime(dlnaRange.End - dlnaRange.Start)
		}
		// passing an NPT duration seems to cause trouble
		// so just pass the "iono" duration
		w.Header().Set(dlna.TimeSeekRangeDomain, dlnaRangeHeader+"/*")
	}
	p, err := misc.Transcode(path_, nptStart, nptLength)
	if err != nil {
		log.Println(err)
		return
	}
	defer p.Close()
	w.WriteHeader(206)
	if n, err := io.Copy(w, p); err != nil {
		// not an error if the peer closes the connection
		func() {
			if opErr, ok := err.(*net.OpError); ok {
				if opErr.Err == syscall.EPIPE {
					return
				}
			}
			log.Println("after copying", n, "bytes:", err)
		}()
	}
}

func init() {
	startTime = time.Now()
}

// Changes the root path shared by the server
func (me *Server) ChangeRootPath(path string) {
	me.RootObjectPath = path
}

func getDefaultFriendlyName() string {
	return fmt.Sprintf("%s: %s on %s", rootDeviceModelName, func() string {
		user, err := user.Current()
		if err != nil {
			panic(err)
		}
		return user.Name
	}(), func() string {
		name, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		return name
	}())
}

func (server *Server) initMux(mux *http.ServeMux) {
	mux.HandleFunc("/", func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("content-type", "text/html")
		err := rootTmpl.Execute(resp, struct {
			Readonly bool
			Path     string
		}{
			true,
			server.RootObjectPath,
		})
		if err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc(contentDirectoryEventSubURL, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "vlc sux", http.StatusNotImplemented)
	})
	mux.HandleFunc(resPath, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			log.Println(err)
		}
		path := r.Form.Get("path")
		if r.Form.Get("transcode") == "" {
			http.ServeFile(w, r, path)
			return
		}
		server.serveDLNATranscode(w, r, path)
	})
	mux.HandleFunc(rootDescPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", `text/xml; charset="utf-8"`)
		w.Header().Set("content-length", fmt.Sprint(len(server.rootDescXML)))
		w.Header().Set("server", serverField)
		w.Write(server.rootDescXML)
	})
	mux.HandleFunc(contentDirectorySCPDURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", `text/xml; charset="utf-8"`)
		http.ServeContent(w, r, ".xml", startTime, bytes.NewReader([]byte(contentDirectoryServiceDescription)))
	})
	mux.HandleFunc(contentDirectoryControlURL, func(w http.ResponseWriter, r *http.Request) {
		soapActionString := r.Header.Get("SOAPACTION")
		soapAction, ok := upnp.ParseActionHTTPHeader(soapActionString)
		if !ok {
			log.Println("invalid soapaction:", soapActionString)
			return
		}
		var env soap.Envelope
		if err := xml.NewDecoder(r.Body).Decode(&env); err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.Header().Set("Ext", "")
		w.Header().Set("Server", serverField)
		actionResponseXML, err := xml.MarshalIndent(func() interface{} {
			argMap, err := server.contentDirectoryResponseArgs(soapAction, env.Body.Action, r.Host)
			if err != nil {
				w.WriteHeader(500)
				return soap.NewFault("UPnPError", err)
			}
			args := make([]soap.Arg, 0, len(argMap))
			for argName, value := range argMap {
				args = append(args, soap.Arg{
					XMLName: xml.Name{Local: argName},
					Value:   value,
				})
			}
			return soap.Action{
				XMLName: xml.Name{
					Space: soapAction.ServiceURN.String(),
					Local: soapAction.Action + "Response",
				},
				Args: args,
			}
		}(), "", "  ")
		if err != nil {
			panic(err)
		}
		body, err := xml.MarshalIndent(soap.NewEnvelope(actionResponseXML), "", "  ")
		if err != nil {
			panic(err)
		}
		body = append([]byte(xml.Header), body...)
		if _, err := w.Write(body); err != nil {
			panic(err)
		}
	})
}

func (srv *Server) Serve() (err error) {
	if srv.FriendlyName == "" {
		srv.FriendlyName = getDefaultFriendlyName()
	}
	srv.httpServeMux = http.NewServeMux()
	srv.rootDeviceUUID = makeDeviceUuid(srv.FriendlyName)
	srv.rootDescXML, err = xml.MarshalIndent(
		upnp.DeviceDesc{
			SpecVersion: upnp.SpecVersion{Major: 1, Minor: 0},
			Device: upnp.Device{
				DeviceType:   rootDeviceType,
				FriendlyName: srv.FriendlyName,
				Manufacturer: "Matt Joiner <anacrolix@gmail.com>",
				ModelName:    rootDeviceModelName,
				UDN:          srv.rootDeviceUUID,
				ServiceList:  services,
			},
		},
		" ", "  ")
	if err != nil {
		return
	}
	srv.rootDescXML = append([]byte(`<?xml version="1.0"?>`), srv.rootDescXML...)
	if err != nil {
		return
	}
	log.Println("HTTP srv on", srv.HTTPConn.Addr())
	srv.initMux(srv.httpServeMux)
	go srv.serveHTTP()
	srv.doSSDP()
	panic("unreachable")
}

// Stops the server
func (me *Server) Close() {
	me.HTTPConn.Close()
}

func didl_lite(chardata string) string {
	return `<DIDL-Lite` +
		` xmlns:dc="http://purl.org/dc/elements/1.1/"` +
		` xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"` +
		` xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"` +
		` xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">` +
		chardata +
		`</DIDL-Lite>`
}

func (me *Server) location(ip net.IP) string {
	url := url.URL{
		Scheme: "http",
		Host: (&net.TCPAddr{
			IP:   ip,
			Port: me.httpPort(),
		}).String(),
		Path: rootDescPath,
	}
	return url.String()
}
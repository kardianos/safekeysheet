package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"html/template"
	"image"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/datamatrix"
	"github.com/kardianos/task"
	"github.com/pkg/browser"
	"github.com/russross/blackfriday/v2"
	kp "github.com/tobischo/gokeepasslib/v3"
	"github.com/vincent-petithory/dataurl"
	"golang.org/x/term"
)

func main() {
	err := task.Start(context.Background(), time.Second*3, run)
	if err != nil {
		log.Fatal(err)
	}
}

const defaultTagName = "safe-print"

func run(ctx context.Context) error {
	var kdbxFn string
	var tagName = defaultTagName
	cmdTree := &task.Command{
		Commands: []*task.Command{
			{
				Name: "gen", Usage: "Generate PDF sheet",
				Flags: []*task.Flag{
					{Name: "kdbx", Usage: "KDBX file to open, config file located in root group named " + tagName + ", config in Notes section.", Type: task.FlagString, Value: &kdbxFn, Required: true},
				},
				Action: generatePDF(&kdbxFn, tagName),
			},
		},
	}

	st := task.DefaultState()
	return task.Run(ctx, st, cmdTree.Exec(os.Args[1:]))
}

func lookGroup(group []kp.Group, ret *[]kp.Entry, tagKey string) {
	for _, gr := range group {
	entryLoop:
		for _, e := range gr.Entries {
			title := e.GetTitle()
			if title == tagKey {
				*ret = append(*ret, e)
				continue entryLoop
			}
			for _, tag := range strings.Fields(e.Tags) {
				if tag == tagKey {
					*ret = append(*ret, e)
					continue entryLoop
				}
			}
		}
		lookGroup(gr.Groups, ret, tagKey)
	}
}

func generatePDF(kdbxFn *string, tagName string) task.Action {
	return task.ActionFunc(func(ctx context.Context, st *task.State, sc task.Script) error {
		ctx, cancel := context.WithTimeout(ctx, time.Second*120)
		defer cancel()

		fmt.Println("Password:")
		pwB, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		password := string(pwB)

		db, err := func(fn, password string) (*kp.Database, error) {
			f, err := os.OpenFile(fn, os.O_RDONLY, 0400)
			if err != nil {
				return nil, err
			}
			defer f.Close()

			db := kp.NewDatabase()
			db.Credentials = kp.NewPasswordCredentials(password)
			err = kp.NewDecoder(f).Decode(db)
			if err != nil {
				return nil, err
			}
			err = db.UnlockProtectedEntries()
			if err != nil {
				return nil, err
			}
			return db, nil
		}(*kdbxFn, password)
		if err != nil {
			return err
		}
		fmt.Println("KDBX File Decrypted")

		matches := func(db *kp.Database, tagKey string) []kp.Entry {
			ret := make([]kp.Entry, 0, 10)
			lookGroup(db.Content.Root.Groups, &ret, tagKey)
			return ret
		}(db, tagName)

		_, dbFn := filepath.Split(*kdbxFn)
		r := root{
			Now: time.Now().Format(time.DateTime),
			KDBX: rootEntry{
				Title:    dbFn,
				Password: password,
			},
		}
		for _, e := range matches {
			re := rootEntry{
				Title:       e.GetTitle(),
				Password:    e.GetPassword(),
				Username:    e.GetContent("UserName"),
				URL:         e.GetContent("URL"),
				Description: e.GetContent("Notes"),
			}
			if re.Title == tagName {
				r.KDBX.Description = re.Description
				r.KDBX.Username = re.Username
				r.KDBX.URL = re.URL
				continue
			}
			r.Entry = append(r.Entry, re)
		}

		buf := &bytes.Buffer{}
		err = renderRoot(buf, r)
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
		xb := buf.Bytes()

		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		defer l.Close()

		go func() {
			<-ctx.Done()
			l.Close()
		}()

		rr := make([]byte, 8)
		rand.Read(rr)
		U := fmt.Sprintf("%X", rr)

		mux := &http.ServeMux{}
		mux.HandleFunc("GET /{U}/print", func(w http.ResponseWriter, r *http.Request) {
			xU := r.PathValue("U")
			if xU != U {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Write(xb)
		})
		mux.HandleFunc("POST /{U}/close", func(w http.ResponseWriter, r *http.Request) {
			xU := r.PathValue("U")
			if xU != U {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			l.Close()
		})
		PU := fmt.Sprintf("http://%s/%s/print", l.Addr().String(), U)
		fmt.Printf("Open %s\n", PU)
		go func() {
			time.Sleep(time.Millisecond * 10)
			browser.OpenURL(PU)
		}()
		srv := http.Server{
			Handler:  mux,
			ErrorLog: log.New(io.Discard, "", 0),
		}
		srv.Serve(l)
		return nil
	})
}

var fmap = map[string]any{
	"datamatrix": func(content string, scale float64) template.URL {
		bar, err := datamatrix.Encode(content)
		if err != nil {
			panic(err)
		}
		sz := bar.Bounds().Size()

		bar, err = barcode.Scale(bar, int(float64(sz.X)*scale), int(float64(sz.Y)*scale))
		if err != nil {
			panic(err)
		}
		img := image.NewGray(bar.Bounds())
		draw.Draw(img, bar.Bounds(), bar, image.Point{}, draw.Src)
		buf := &bytes.Buffer{}
		err = png.Encode(buf, img)
		if err != nil {
			panic(err)
		}
		return template.URL(dataurl.New(buf.Bytes(), "image/png").String())
	},
	"markdown": func(text string) template.HTML {
		out := blackfriday.Run([]byte(text))
		return template.HTML(out)
	},
}

func renderRoot(w io.Writer, r root) error {
	t, err := template.New("").Funcs(fmap).Parse(htmlFile)
	if err != nil {
		return err
	}
	return t.Execute(w, r)
}

type rootEntry struct {
	Title       string
	Password    string
	Username    string
	URL         string
	Description string
}

type root struct {
	Now   string
	KDBX  rootEntry
	Entry []rootEntry
}

const htmlFile = `<!DOCTYPE html>

<style>
.ins {
	padding: 8px;
}
.barcode {
	padding: 16px;
}
table {
	break-inside: avoid;
}
td {
	vertical-align: top;
}
label {
	font-weight: bold;
}
.value {
	overflow-wrap: anywhere;
	font-family: monospace;
	margin-left: 8px;
}
table {
	width: 100%;
}
th.top {
	border-top: 3px solid black;
}
td.left {
	width: 35%;
}
td.right {
	border-left: 1px solid grey;
}
</style>

{{define "pass"}}
{{if $}}
	<img class="barcode" src="{{datamatrix $ 4}}">
{{end}}
{{end}}

{{define "item"}}
<table>
<tr>
	<th colspan=2 class=top>{{.Title}}
<tr>
	<td class=left>
		{{if .Username}}<div><label>Username:</label> <div class=value>{{.Username}}</div></div>{{end}}
		{{if .Password}}<div><label>Password:</label> <div class=value>{{.Password}}</div></div>{{end}}
		{{template "pass" .Password}}
	<td class=right>
		<div class=ins>
			{{markdown .Description}}
		</div>
		{{if .URL}}<div><label>URL:</label> <div class=value>{{.URL}}</div></div>{{end}}
</table>
{{end}}

<h1>Created {{.Now}}</h1>

{{template "item" .KDBX}}

{{range .Entry}}
	{{template "item" .}}
{{end}}

<script>
window.onafterprint = function() {
	fetch("./close", {
		method: "post",
	})
	.finally( (response) => {
		console.log("CLOSING");
		window.close();
	});
};
window.print();
</script>
`

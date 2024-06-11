//go:build ignore
// +build ignore

package main

import (
	"bufio"
	"flag"
	"io"
	"os"
	"time"

	"github.com/anacrolix/log"

	"github.com/anacrolix/dms/misc"
)

func main() {
	ss := flag.String("ss", "", "")
	t := flag.String("t", "", "")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Println("wrong argument count")
		os.Exit(1)
	}
	r, err := misc.Transcode(flag.Arg(0), *ss, *t)
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	go func() {
		buf := bufio.NewWriterSize(os.Stdout, 1234)
		n, err := io.Copy(buf, r)
		log.Println("copied", n, "bytes")
		if err != nil {
			log.Println(err)
		}
	}()
	time.Sleep(time.Second)
	go r.Close()
	time.Sleep(time.Second)
}

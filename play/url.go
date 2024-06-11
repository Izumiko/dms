//go:build ignore
// +build ignore

package main

import (
	"fmt"
	"github.com/anacrolix/log"
	"net/url"
	"os"
)

func main() {
	url_, err := url.Parse("[192:168:26:2::3]:1900")
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	fmt.Printf("%#v\n", url_)
}

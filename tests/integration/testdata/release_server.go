package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	log.Fatal(http.ListenAndServe("127.0.0.1:22221", http.FileServer(http.Dir(os.Args[1]))))
}

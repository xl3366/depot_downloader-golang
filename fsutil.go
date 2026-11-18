package main

import (
    "log"
    "os"
)

func removePath(p string) {
    fi, err := os.Lstat(p)
    if err != nil { return }
    if fi.Mode()&os.ModeSymlink != 0 {
        os.Remove(p)
        return
    }
    if fi.IsDir() {
        if err := os.Remove(p); err != nil {
            log.Printf("remove dir %s: %v", p, err)
        }
        return
    }
    os.Remove(p)
}
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
)

type neighbor struct {
	ipAddr  int32  // TODO: ?
	udpAddr int32  // TODO: ?
	name    string // TODO ?
}

/*
 * Read LNX file and initialize neighbors, table, etc.
 */
func Initialize(filePath string) (err error) {
	file, err := os.Open(filePath)
	if err != nil {
		return errors.New("file open fail")
	}
	defer file.Close()

	// read from file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return errors.New("scanner fail")
	}
	return nil
}

func CreateInterface() {

}

func CreateForwardingTable() {

}

func GetNeighborList() {

}

func EnableInterface() {

}

func DisableInterface() {

}

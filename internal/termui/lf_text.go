package termui

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

var errLFNonText = errors.New("File does not look like text")

func checkLFText(name string) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errLFNonText
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	var sample [4096]byte
	n, err := io.ReadFull(file, sample[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	data := sample[:n]
	var order binary.ByteOrder
	if len(data) >= 2 {
		switch binary.BigEndian.Uint16(data) {
		case 0xfeff:
			order = binary.BigEndian
		case 0xfffe:
			order = binary.LittleEndian
		}
	}
	if order != nil {
		for data = data[2:]; len(data) >= 2; data = data[2:] {
			if lfBinaryControl(order.Uint16(data)) {
				return errLFNonText
			}
		}
		return nil
	}
	for _, b := range data {
		if lfBinaryControl(uint16(b)) {
			return errLFNonText
		}
	}
	return nil
}

func lfBinaryControl(value uint16) bool {
	switch value {
	case '\t', '\n', '\f', '\r':
		return false
	default:
		return value < 0x20 || value == 0x7f
	}
}

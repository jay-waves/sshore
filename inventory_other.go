//go:build !linux

package main

func localSessionInventory() (inventory, error) {
	inv := emptyInventory()
	var e error
	inv.Peers, e = collectPeerViews()
	return inv, e
}

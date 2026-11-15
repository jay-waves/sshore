//go:build !linux

package main

func localSessionInventory(details ...bool) (inventory, error) {
	inv := emptyInventory()
	var e error
	inv.Peers, e = collectPeerViews()
	return inv, e
}

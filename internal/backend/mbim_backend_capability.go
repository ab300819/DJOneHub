package backend

import "github.com/ab300819/DJOneHub/pkg/mbim"

func (b *MBIMBackend) Capability() *mbim.Capabilities {
	return b.source.Capability()
}

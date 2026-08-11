# Third-Party Notices

DJOneHub contains code derived from the upstream VoHive project and retains the license and required notice provided in the repository root [`LICENSE`](LICENSE):

```text
Required Notice: Copyright iniwex5 (https://github.com/iniwex5/vohive)
```

## Release Runtime

The macOS release package includes **libusb 1.0.30**, distributed under the GNU Lesser General Public License, version 2.1 or later.

- Project: <https://libusb.info/>
- Source: <https://github.com/libusb/libusb/releases/tag/v1.0.30>
- License text in the release package: `licenses/libusb-COPYING`

## Source Dependencies

All Go dependencies are resolved through the standard module system and pinned by
`go.mod` and `go.sum`. Reproducibility comes from those checksums rather than from
copies committed to this repository.

Key components and their upstreams:

| Component | Upstream |
| --- | --- |
| euicc-go | <https://github.com/damonto/euicc-go> |
| uicc-go | <https://github.com/damonto/uicc-go> |
| quectel-qmi-go | <https://github.com/iniwex5/quectel-qmi-go> |
| strftime | <https://github.com/lestrrat-go/strftime> |
| pkg/errors | <https://github.com/pkg/errors> |
| golang.org/x/sys | <https://pkg.go.dev/golang.org/x/sys> |
| golang.org/x/text | <https://pkg.go.dev/golang.org/x/text> |
| multierr | <https://github.com/uber-go/multierr> |

Dependencies fetched through Go modules retain their own licenses and copyright notices. This file is informational and does not replace any component's full license text.

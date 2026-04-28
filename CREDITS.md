# Credits and acknowledgements

NocordHF stands on the shoulders of the amateur-radio software
community. The FT8 modem in `lib/ft8/` is an independent Go implementation of the public FT8 protocol — the protocol's wire
format (LDPC generator and parity-check matrices, Costas sync
pattern, CRC-14 polynomial, GFSK pulse shape, callsign hash
multiplier, etc.) is a published specification that every conforming
FT8 implementation must implement identically; it's documented in:

> Steven J. Franke (K9AN), Bill Somerville (G4WJS), and Joseph H.
> Taylor Jr. (K1JT). **"The FT4 and FT8 Communication Protocols."**
> *QEX*, July/August 2020.
> [Princeton mirror](https://physics.princeton.edu/pulsar/k1jt/FT4_FT8_QEX.pdf)

## Thanks

- **Joseph H. Taylor Jr. (K1JT) and the WSJT Development Group** for
  designing FT8 and for the WSJT-X reference implementation.
  https://wsjt.sourceforge.io/
- **Kārlis Goba (YL3JG)** for [`ft8_lib`](https://github.com/kgoba/ft8_lib),
  the embedded-friendly C reference whose architecture inspired
  several decoder organisation choices in this project.

## Other dependencies

NocordHF builds on a number of excellent Go libraries; see `go.mod`
for the full list. Notable runtime dependencies include
[Fyne](https://fyne.io/) (UI), [malgo](https://github.com/gen2brain/malgo)
(audio capture), [serial](https://github.com/bugst/go-serial) (CAT
control), and [gonum](https://gonum.org/) (FFT).

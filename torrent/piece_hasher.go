package torrent

type pieceHasher struct {
	t           *Torrent
	numVerified int
}

func (p *pieceHasher) Run() {
	for {
		select {
		case piece := <-p.t.pieceQueuedHashingCh:
			correct := p.t.storage.HashPiece(piece, p.t.pieceLen(uint32(piece)))
			p.t.pieceHashedCh <- pieceHashed{
				pieceIndex: piece,
				ok:         correct,
			}
			if correct {
				p.numVerified++
				if p.numVerified == p.t.numPieces() {
					return
				}
			}
		case <-p.t.closed:
			return
		}
	}
}

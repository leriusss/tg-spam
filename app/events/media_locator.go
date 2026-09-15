package events

import (
	"strings"

	tbapi "github.com/OvyFlash/telegram-bot-api"

	"github.com/umputun/tg-spam/app/storage"
)

type mediaLocatorStatus string

const (
	mediaLocatorExtracted   mediaLocatorStatus = "extracted"
	mediaLocatorUnavailable mediaLocatorStatus = "unavailable"
	mediaLocatorUnsupported mediaLocatorStatus = "unsupported"
	mediaLocatorAlbum       mediaLocatorStatus = "album_deferred"
)

// extractMediaLocatorKey extracts a deterministic Phase 2A key without network or filesystem access.
func extractMediaLocatorKey(msg *tbapi.Message) (storage.MediaLocatorKey, mediaLocatorStatus) {
	if msg == nil {
		return storage.MediaLocatorKey{}, mediaLocatorUnavailable
	}
	if msg.MediaGroupID != "" {
		return storage.MediaLocatorKey{}, mediaLocatorAlbum
	}

	var key storage.MediaLocatorKey
	switch {
	case len(msg.Photo) > 0:
		photo := canonicalPhotoSize(msg.Photo)
		key = storage.MediaLocatorKey{Version: storage.MediaLocatorVersion, Kind: storage.MediaPhoto, StableMediaID: photo.FileUniqueID}
	case msg.Animation != nil:
		key = storage.MediaLocatorKey{Version: storage.MediaLocatorVersion, Kind: storage.MediaAnimation, StableMediaID: msg.Animation.FileUniqueID}
	case msg.Video != nil:
		key = storage.MediaLocatorKey{Version: storage.MediaLocatorVersion, Kind: storage.MediaVideo, StableMediaID: msg.Video.FileUniqueID}
	case msg.Document != nil:
		key = storage.MediaLocatorKey{Version: storage.MediaLocatorVersion, Kind: storage.MediaDocument, StableMediaID: msg.Document.FileUniqueID}
	case msg.Audio != nil || msg.Voice != nil || msg.VideoNote != nil || msg.Sticker != nil || msg.Story != nil || msg.PaidMedia != nil:
		return storage.MediaLocatorKey{}, mediaLocatorUnsupported
	default:
		return storage.MediaLocatorKey{}, mediaLocatorUnavailable
	}
	if strings.TrimSpace(key.StableMediaID) == "" {
		return storage.MediaLocatorKey{}, mediaLocatorUnavailable
	}
	return key, mediaLocatorExtracted
}

func canonicalPhotoSize(sizes []tbapi.PhotoSize) tbapi.PhotoSize {
	best := sizes[0]
	for _, candidate := range sizes[1:] {
		bestArea := int64(best.Width) * int64(best.Height)
		candidateArea := int64(candidate.Width) * int64(candidate.Height)
		if candidateArea > bestArea ||
			(candidateArea == bestArea && candidate.Width > best.Width) ||
			(candidateArea == bestArea && candidate.Width == best.Width && candidate.Height > best.Height) ||
			(candidateArea == bestArea && candidate.Width == best.Width && candidate.Height == best.Height && candidate.FileUniqueID > best.FileUniqueID) {
			best = candidate
		}
	}
	return best
}

func storageIdentity(origin adminReportIdentity) (storage.LocatorIdentity, bool) {
	switch origin.Kind {
	case adminReportIdentityUser:
		if origin.ID > 0 {
			return storage.LocatorIdentity{Kind: storage.LocatorIdentityUser, ID: origin.ID}, true
		}
	case adminReportIdentitySenderChat:
		if origin.ID < 0 {
			return storage.LocatorIdentity{Kind: storage.LocatorIdentitySenderChat, ID: origin.ID}, true
		}
	}
	return storage.LocatorIdentity{}, false
}

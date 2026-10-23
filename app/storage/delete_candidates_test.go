package storage

import (
	"context"
	"fmt"
	"time"
)

func (s *StorageTestSuite) TestDeleteCandidatesScopedDuplicatesAndMedia() {
	ctx := context.Background()
	for _, fixture := range s.getTestDB() {
		s.Run(fmt.Sprint(fixture.Type), func() {
			db := fixture.DB
			loc, err := NewLocator(ctx, time.Hour, 1000, db)
			s.Require().NoError(err)
			defer db.Exec("DROP TABLE messages")
			defer db.Exec("DROP TABLE spam")
			for _, chat := range []int64{-100, -200} {
				for _, mid := range []int{101, 102} {
					s.Require().NoError(loc.AddMessage(ctx, "same text", chat, 7, "author", mid))
				}
			}
			rows, err := loc.DeleteCandidates(ctx, "same text", nil, -100, 21)
			s.Require().NoError(err)
			s.Require().Len(rows, 2)
			s.Equal(102, rows[0].MsgID)
			s.Equal(101, rows[1].MsgID)
			// Existing legacy hash and the new scoped hash must not produce two candidates.
			_, err = db.Exec(db.Adopt(`INSERT INTO messages VALUES (?,?,?,?,?,?,?)`), loc.MsgHash("same text")+":101", db.GID(), time.Now(), int64(-100), int64(7), "author", 101)
			s.Require().NoError(err)
			rows, err = loc.DeleteCandidates(ctx, "same text", nil, -100, 21)
			s.Require().NoError(err)
			s.Len(rows, 2)
			key := MediaLocatorKey{Version: 1, Kind: MediaPhoto, StableMediaID: "same-photo"}
			for _, chat := range []int64{-100, -200} {
				for _, mid := range []int{103, 104} {
					s.Require().NoError(loc.AddMediaMessage(ctx, key, chat, LocatorIdentity{Kind: LocatorIdentityUser, ID: 7}, "", mid))
				}
			}
			rows, err = loc.DeleteCandidates(ctx, "", &key, -100, 21)
			s.Require().NoError(err)
			s.Len(rows, 2)
			// A foreign gid with the same content/chat/message IDs cannot overwrite these originals.
			_, err = db.Exec(db.Adopt(`INSERT INTO messages VALUES (?,?,?,?,?,?,?)`), loc.MsgHash("same text")+":other:-100:102", "other", time.Now(), int64(-100), int64(9), "foreign", 102)
			s.Require().NoError(err)
			rows, err = loc.DeleteCandidates(ctx, "same text", nil, -100, 21)
			s.Require().NoError(err)
			s.Require().Len(rows, 2)
			s.EqualValues(7, rows[0].UserID)
		})
	}
}

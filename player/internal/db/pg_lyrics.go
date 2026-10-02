package db

import "database/sql"

func (s *PGStore) GetLyrics(trackID int64) (*Lyrics, bool, error) {
	row := s.DB.QueryRow(`
SELECT track_id, plain_lyrics, synced_lyrics, source, source_id,
       instrumental, status, COALESCE(error,''), updated_at
FROM lyrics WHERE track_id = $1`, trackID)
	var ly Lyrics
	err := row.Scan(
		&ly.TrackID, &ly.PlainLyrics, &ly.SyncedLyrics, &ly.Source, &ly.SourceID,
		&ly.Instrumental, &ly.Status, &ly.Error, &ly.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &ly, true, nil
}

package domain

type EntityMode int

const (
	ModeUnknown EntityMode = iota
	ModeDisplay
	ModeInsert
	ModeUpdate
	ModeDelete
	ModeArchive
	ModeUnarchive
)

func (m EntityMode) String() string {
	switch m {
	case ModeDisplay:
		return "DISPLAY"
	case ModeInsert:
		return "INSERT"
	case ModeUpdate:
		return "UPDATE"
	case ModeDelete:
		return "DELETE"
	case ModeArchive:
		return "ARCHIVE"
	case ModeUnarchive:
		return "UNARCHIVE"
	default:
		return "UNKNOWN"
	}
}

func (m EntityMode) IsValid() bool {
	return m >= ModeDisplay && m <= ModeUnarchive
}

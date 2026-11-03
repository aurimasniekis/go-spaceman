package proto

// Memory flush before a backup (protocol minor 5, ADR-0045). Before central snapshots a backup it sends memory.flush to
// every connected node that negotiated minor ≥ 5, naming the spaces pinned to that node. The node checkpoints every
// instance of those spaces' role memory, syncs each collection to central and waits, at most WaitSeconds, for central's
// acknowledgements; then it answers memory.flushed with one entry per requested space. It is not a command: nothing is
// journaled and no lease is needed. A lost request or answer only means that the backup records the space's memory as
// possibly stale; the node does nothing it would not also do on its own (checkpoints and syncs).

// MinorMemoryFlush is the first minor version that carries memory.flush.
const MinorMemoryFlush = 5

// MemoryFlushMaxWait bounds WaitSeconds; a larger or missing value is clamped by the node.
const MemoryFlushMaxWait = 600

// MemoryFlushMaxSpaces bounds the spaces one request names.
const MemoryFlushMaxSpaces = 1000

// MemoryFlush asks a node to checkpoint and sync the role memory of the listed spaces (memory.flush).
type MemoryFlush struct {
	FlushID     string   `json:"flush_id"`
	SpaceIDs    []string `json:"space_ids"`
	WaitSeconds int      `json:"wait_seconds"`
}

// MemoryFlushed answers a MemoryFlush (memory.flushed): one entry per requested space.
type MemoryFlushed struct {
	FlushID string             `json:"flush_id"`
	Spaces  []MemoryFlushSpace `json:"spaces"`
}

// MemoryFlushSpace is one space's flush outcome. Flushed means that, before the wait ended, every collection of the
// space on the node had no uncheckpointed files, no commits central had not acknowledged and no sync in flight (a
// space without role memory on the node is flushed trivially). Detail explains a false Flushed in a few words.
type MemoryFlushSpace struct {
	SpaceID     string `json:"space_id"`
	Flushed     bool   `json:"flushed"`
	Collections int    `json:"collections"`
	Detail      string `json:"detail,omitempty"`
}

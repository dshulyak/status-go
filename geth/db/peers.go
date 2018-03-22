package db

import (
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// PeersDatabase maintains list of peers that were discovered.
type PeersDatabase struct {
	db *leveldb.DB
}

func makePeerKey(peer *discv5.Node, topic discv5.Topic) []byte {
	topicLth := len([]byte(topic))
	lth := topicLth + len(peer.ID)
	key := make([]byte, lth)
	copy(key[:], []byte(topic)[:])
	copy(key[topicLth:], peer.ID[:])
	return key
}

// AddPeer stores peer with a following key: <topic><peer ID>
func (d *PeersDatabase) AddPeer(peer *discv5.Node, topic discv5.Topic) error {
	data, err := peer.MarshalText()
	if err != nil {
		return err
	}
	return d.db.Put(makePeerKey(peer, topic), data, nil)
}

// RemovePeer deletes a peer from database.
func (d *PeersDatabase) RemovePeer(peer *discv5.Node, topic discv5.Topic) error {
	return d.db.Delete(makePeerKey(peer, topic), nil)
}

// GetPeers returns peers for a given topic with a limit.
func (d *PeersDatabase) GetPeersRange(topic discv5.Topic, limit int) (nodes []*discv5.Node) {
	topicLth := len([]byte(topic))
	key := make([]byte, topicLth)
	copy(key[:], []byte(topic))
	iterator := d.db.NewIterator(&util.Range{Start: key}, nil)
	defer iterator.Release()
	count := 0
	for iterator.Next() {
		node := discv5.Node{}
		value := iterator.Value()
		if err := node.UnmarshalText(value); err != nil {
			log.Error("can't unmarshal node", "value", value, "error", err)
			continue
		}
		nodes = append(nodes, &node)
		count++
		if count == limit {
			return nodes
		}
	}
	return nodes
}

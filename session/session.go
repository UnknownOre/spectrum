package session

import (
	"errors"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/spectrum-proxy/spectrum/internal"
	"github.com/spectrum-proxy/spectrum/server"
	"github.com/spectrum-proxy/spectrum/session/animation"
	"sync"
	"sync/atomic"
)

type Session struct {
	clientConn *minecraft.Conn

	serverAddr string
	serverConn *server.Conn
	serverMu   sync.RWMutex

	logger   internal.Logger
	registry *Registry

	handler   Handler
	tracker   *Tracker
	animation animation.Animation

	latency      int64
	once         sync.Once
	transferring atomic.Bool
}

func NewSession(clientConn *minecraft.Conn, logger internal.Logger, registry *Registry, addr string, latencyInterval int64) (s *Session, err error) {
	s = &Session{
		clientConn: clientConn,

		logger:   logger,
		registry: registry,

		handler:   NoopHandler{},
		tracker:   NewTracker(),
		animation: &animation.Dimension{},
		latency:   0,
	}

	go func() {
		serverConn, err := s.Dial(addr)
		s.serverAddr = addr
		s.serverConn = serverConn
		if err != nil {
			s.Close()
			s.logger.Errorf("Failed to dial server: %v", err)
			return
		}

		if err := clientConn.StartGame(serverConn.GameData()); err != nil {
			s.Close()
			s.logger.Errorf("Failed to start game timeout: %v", err)
			return
		}

		s.sendMetadata(true)
		for _, pk := range serverConn.ReadDeferred() {
			_ = clientConn.WritePacket(pk)
		}

		go handleIncoming(s)
		go handleOutgoing(s)
		go handleLatency(s, latencyInterval)

		s.registry.AddSession(clientConn.IdentityData().XUID, s)
		s.logger.Infof("Successfully started session for %s", clientConn.IdentityData().DisplayName)
	}()
	return
}

func (s *Session) Dial(addr string) (*server.Conn, error) {
	clientConn := s.clientConn
	d := server.Dialer{
		Origin:       clientConn.RemoteAddr().String(),
		ClientData:   clientConn.ClientData(),
		IdentityData: clientConn.IdentityData(),
	}
	return d.Dial(addr)
}

func (s *Session) Transfer(addr string) error {
	if !s.transferring.CompareAndSwap(false, true) {
		return errors.New("already transferring")
	}

	s.serverMu.Lock()
	defer func() {
		s.serverMu.Unlock()
		s.transferring.Store(false)
	}()

	s.sendMetadata(true)
	conn, err := s.Dial(addr)
	if err != nil {
		s.sendMetadata(false)
		s.logger.Errorf("Failed to dial server: %v", err)
		return err
	}

	serverGameData := conn.GameData()
	s.animation.Play(s.clientConn, serverGameData)

	chunk := emptyChunk(serverGameData.Dimension)
	pos := serverGameData.PlayerPosition
	chunkX := int32(pos.X()) >> 4
	chunkZ := int32(pos.Z()) >> 4
	for x := chunkX - 4; x <= chunkX+4; x++ {
		for z := chunkZ - 4; z <= chunkZ+4; z++ {
			_ = s.clientConn.WritePacket(&packet.LevelChunk{
				Dimension:     packet.DimensionNether,
				Position:      protocol.ChunkPos{x, z},
				SubChunkCount: 1,
				RawPayload:    chunk,
			})
		}
	}

	s.tracker.clearEffects(s)
	s.tracker.clearEntities(s)
	s.tracker.clearBossBars(s)
	s.tracker.clearPlayers(s)
	s.tracker.clearScoreboards(s)

	_ = s.clientConn.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: serverGameData.EntityRuntimeID,
		Position:        serverGameData.PlayerPosition,
		Pitch:           serverGameData.Pitch,
		Yaw:             serverGameData.Yaw,
		Mode:            packet.MoveModeReset,
	})

	_ = s.clientConn.WritePacket(&packet.LevelEvent{
		EventType: packet.LevelEventStopRaining,
		EventData: 10_000,
	})
	_ = s.clientConn.WritePacket(&packet.LevelEvent{
		EventType: packet.LevelEventStopThunderstorm,
	})

	_ = s.clientConn.WritePacket(&packet.SetDifficulty{
		Difficulty: uint32(serverGameData.Difficulty),
	})
	_ = s.clientConn.WritePacket(&packet.SetPlayerGameType{
		GameType: serverGameData.PlayerGameMode,
	})

	_ = s.clientConn.WritePacket(&packet.GameRulesChanged{
		GameRules: serverGameData.GameRules,
	})

	s.animation.Clear(s.clientConn, serverGameData)
	s.serverConn.Close()

	s.serverAddr = addr
	s.serverConn = conn

	for _, pk := range conn.ReadDeferred() {
		_ = s.clientConn.WritePacket(pk)
	}
	s.logger.Debugf("Transferred session for %s to %s", s.clientConn.IdentityData().DisplayName, addr)
	return nil
}

func (s *Session) SetHandler(handler Handler) {
	s.handler = handler
}

func (s *Session) SetAnimation(animation animation.Animation) {
	s.animation = animation
}

func (s *Session) Disconnect(message string) {
	_ = s.clientConn.WritePacket(&packet.Disconnect{
		Message: message,
	})
	s.Close()
}

func (s *Session) Server() *server.Conn {
	s.serverMu.RLock()
	defer s.serverMu.RUnlock()
	return s.serverConn
}

func (s *Session) Latency() int64 {
	return s.clientConn.Latency().Milliseconds() + s.latency
}

func (s *Session) Close() {
	s.once.Do(func() {
		_ = s.clientConn.Close()

		if s.serverConn != nil {
			s.serverConn.Close()
		}

		identity := s.clientConn.IdentityData()
		s.registry.RemoveSession(identity.XUID)
		s.logger.Infof("Closed session for %s", identity.DisplayName)
	})
}

func (s *Session) sendMetadata(noAI bool) {
	metadata := protocol.NewEntityMetadata()
	if noAI {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagNoAI)
	}
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagBreathing)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
	_ = s.clientConn.WritePacket(&packet.SetActorData{
		EntityRuntimeID: s.clientConn.GameData().EntityRuntimeID,
		EntityMetadata:  metadata,
	})
}

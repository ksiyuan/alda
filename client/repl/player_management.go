package repl

import (
	"fmt"
	"strings"
	"time"

	"alda.io/client/emitter"
	log "alda.io/client/logging"
	"alda.io/client/system"
	"alda.io/client/util"
)

const findPlayerTimeout = 20 * time.Second
const playerPoolFillInterval = 15 * time.Second
const pingTimeout = 5 * time.Second
const pingInterval = 1 * time.Second
const failedPingThreshold = 3

func findAvailablePlayer() (system.PlayerState, error) {
	var player system.PlayerState

	if err := util.Await(
		func() error {
			availablePlayer, err := system.FindAvailablePlayer()
			if err != nil {
				return err
			}

			player = availablePlayer
			return nil
		},
		findPlayerTimeout,
	); err != nil {
		return system.PlayerState{}, err
	}

	return player, nil
}

func (server *Server) emitter() (emitter.OSCEmitter, error) {
	if !server.hasPlayer() {
		return emitter.OSCEmitter{}, fmt.Errorf("no player process is available")
	}

	return emitter.OSCEmitter{Port: server.player.Port}, nil
}

// Player management happens asynchronously (see the loop in `managePlayers`),
// so at any given moment, it is probable, but not 100% certain, that a player
// process will be available. This function handles the boilerplate of waiting
// for a player process to be available, constructing an OSCEmitter that will
// emit to that player's port, and then running `execute`, a function that uses
// the OSCEmitter.
func (server *Server) withEmitter(
	execute func(emitter.OSCEmitter) error,
) error {
	var emitter emitter.OSCEmitter

	if err := util.Await(
		func() error {
			oe, err := server.emitter()
			if err != nil {
				return err
			}

			emitter = oe
			return nil
		},
		findPlayerTimeout,
	); err != nil {
		return err
	}

	return execute(emitter)
}

// Boilerplate to overcome the slight awkwardness of Go's zero value semantics
// for structs. We can't set `server.player` to nil because a struct can't be
// nil, so the best we can do is set it to an empty struct
// (`system.PlayerState{}`), which means all the struct fields have zero values
// (ID="", Port=0, etc.)
//
// For practical purposes, if Port is 0, then we can be reasonably certain that
// the server doesn't have a player to talk to.
func (server *Server) hasPlayer() bool {
	return server.player.Port != 0
}

// The `managePlayers` loop regularly checks to see if the player process that
// the server is using is still reachable. If the player process ever disappears
// or becomes unreachable, the `managePlayers` loop recovers by finding another
// player process to replace it.
//
// To signal that part of the loop, we "unset" `server.player` by setting it to
// the zero value (`system.PlayerState{}`). At that point, `server.hasPlayer()`
// will return false, and the player process will be replaced and
// `server.player` will be set to the current state of the new player process.
func (server *Server) unsetPlayer() {
	server.player = system.PlayerState{}
}

// The server has two responsibilities when it comes to managing player
// processes:
//
// 1. Ensuring that the "player pool" is full, i.e. that there is always a fresh
//    player process available to use if needed, e.g. if the one that the server
//    is using falls over / becomes unavailable.
//
// 2. Ensuring that there is one specific player process available for the
//    server to use, and that that process remains available for as long as the
//    server needs to use it. The server does this by sending a `/ping` message
//    to the player at regular intervals. If the player becomes unresponsive,
//    the server is responsible for recovering by switching to use another
//    player process.
func (server *Server) managePlayers() {
	playerPoolLastFilled := time.Unix(0, 0)
	lastPing := time.Unix(0, 0)

	for {
		now := time.Now()

		// Fill the player pool.
		if now.Sub(playerPoolLastFilled) > playerPoolFillInterval {
			if err := system.FillPlayerPool(); err != nil {
				log.Warn().Err(err).Msg("Failed to fill player pool.")
			} else {
				log.Debug().Msg("Filled player pool.")
			}

			playerPoolLastFilled = now
		}

		// If the server already has a player process that it's using, fetch updated
		// state information about that player process.
		if server.hasPlayer() {
			updatedState, err := system.FindPlayerByID(server.player.ID)

			if err == nil {
				server.player = updatedState
			} else if strings.HasPrefix(err.Error(), "player not found") {
				// If the state information tells us that the player process no longer
				// exists, then we forget about that player process and a new one will be
				// found to replace it shortly.
				log.Warn().
					Interface("player", server.player).
					Msg("Player process is offline.")
				server.unsetPlayer()
			} else {
				log.Warn().Err(err).Msg("Failed to update player state information.")
			}
		}

		if !server.hasPlayer() {
			player, err := findAvailablePlayer()
			if err != nil {
				log.Warn().Err(err).Msg("No player processes available.")
			} else {
				log.Info().Interface("player", player).Msg("Found player process.")
				server.player = player
			}
		}

		if server.hasPlayer() && now.Sub(lastPing) > pingInterval {
			// We can safely ignore `err` here because it should always be nil, given
			// that we just checked that `server.hasPlayer()` is true.
			emitter, _ := server.emitter()

			if err := util.Await(
				func() error { return emitter.EmitPingMessage() },
				pingTimeout,
			); err != nil {
				log.Warn().
					Err(err).
					Interface("player", server.player).
					Msg("Player process unreachable.")

				server.unsetPlayer()
			} else {
				log.Debug().
					Interface("player", server.player).
					Msg("Sent ping to player process.")
			}

			lastPing = now
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func (server *Server) shutdownPlayer() error {
	if err := server.withEmitter(func(emitter emitter.OSCEmitter) error {
		return emitter.EmitShutdownMessage(0)
	}); err != nil {
		return err
	}

	// Now we un-set the player so that we don't accidentally keep trying to use
	// the same player process while it's in the process of shutting down. (This
	// might also speed up the process of the `managePlayers` loop discovering
	// that there is no player available, prompting it to find a replacement.)
	//
	// (Technically, there is still a potential race condition here where the
	// `managePlayers` loop un-sets the player before we get to this line, so
	// we double-unset it. But the risk is low because even if that happens, the
	// worst case scenario is that we would end up replacing the player twice, and
	// even if that happens, we would still end up with a player to use below.)
	server.unsetPlayer()

	return nil
}

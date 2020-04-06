package featureflags

import (
	"fmt"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/jonas747/yagpdb/common"
	"github.com/mediocregopher/radix/v3"
)

// PluginWithFeatureFlags is a interface for plugins that provide their own feature-flags
type PluginWithFeatureFlags interface {
	common.Plugin

	UpdateFeatureFlags(guildID int64) ([]string, error)
	AllFeatureFlags() []string
}

var (
	cache  = make(map[int64][]string)
	cacheL sync.RWMutex
)

func keyGuildFlags(guildID int64) string {
	return fmt.Sprintf("f_flags:%d", guildID)
}

// GetGuildFlags returns the feature flags a guild has
func GetGuildFlags(guildID int64) ([]string, error) {
	// fast path
	cacheL.RLock()
	if flags, ok := cache[guildID]; ok {
		cacheL.Unlock()
		return flags, nil
	}
	cacheL.RUnlock()

	// need to fetch from redis, upgrade lock
	cacheL.Lock()
	defer cacheL.Unlock()

	var result []string
	err := common.RedisPool.Do(radix.Cmd(&result, "SMEMBERS", keyGuildFlags(guildID)))
	if err != nil {
		return nil, errors.WithStackIf(err)
	}

	cache[guildID] = result
	return result, nil
}

// GuildHasFlag returns true if the target guild has the provided flag
func GuildHasFlag(guildID int64, flag string) (bool, error) {
	flags, err := GetGuildFlags(guildID)
	if err != nil {
		return false, err
	}

	return common.ContainsStringSlice(flags, flag), nil
}

// UpdateGuildFlags updates the provided guilds feature flags
func UpdateGuildFlags(guildID int64) error {
	keyLock := fmt.Sprintf("feature_flags_updating:%d", guildID)
	err := common.BlockingLockRedisKey(keyLock, time.Second*60, 60)
	if err != nil {
		return errors.WithStackIf(err)
	}

	defer common.UnlockRedisKey(keyLock)

	var lastErr error
	for _, p := range common.Plugins {
		if cast, ok := p.(PluginWithFeatureFlags); ok {
			err := updatePluginFeatureFlags(guildID, cast)
			if err != nil {
				lastErr = err
			}
		}
	}

	return lastErr
}

func updatePluginFeatureFlags(guildID int64, p PluginWithFeatureFlags) error {

	allFlags := p.AllFeatureFlags()

	activeFlags, err := p.UpdateFeatureFlags(guildID)
	if err != nil {
		return errors.WithStackIf(err)
	}

	toDel := make([]string, 0)
	for _, v := range allFlags {
		if common.ContainsStringSlice(activeFlags, v) {
			continue
		}

		// flag isn't active
		toDel = append(toDel, v)
	}

	filtered := make([]string, 0, len(activeFlags))

	// make sure all flags are valid
	for _, v := range activeFlags {
		if !common.ContainsStringSlice(allFlags, v) {
			logger.WithError(err).Errorf("Flag %q is not in the spec of %s", v, p.PluginInfo().SysName)
		} else {
			filtered = append(filtered, v)
		}
	}

	key := keyGuildFlags(guildID)

	err = common.RedisPool.Do(radix.WithConn(key, func(conn radix.Conn) error {

		// apply the added/unchanged flags first
		err := conn.Do(radix.Cmd(nil, "SADD", append([]string{key}, filtered...)...))
		if err != nil {
			return errors.WithStackIf(err)
		}

		// then remove the flags we don't have
		err = conn.Do(radix.Cmd(nil, "SREM", append([]string{key}, toDel...)...))
		if err != nil {
			return errors.WithStackIf(err)
		}

		return nil
	}))

	if err != nil {
		return errors.WithStackIf(err)
	}

	return nil
}
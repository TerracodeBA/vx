package service

import "github.com/TerracodeBA/vx/internal/rules"

// jobDefsOf turns a repository's check definitions into what the queue
// needs. Until rules.CheckDef lands (r5-core), there are none.
func jobDefsOf(rs rules.Rules) []jobDef {
	_ = rs
	return nil
}

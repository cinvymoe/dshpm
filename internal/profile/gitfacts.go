package profile

import (
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
)

// FillLocalGitFacts enriches a local-source plugin with git state.
func FillLocalGitFacts(plugin *model.Plugin) {
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		return
	}
	plugin.GitURL = gitutil.RemoteURL(plugin.LocalDir, "origin")
	if plugin.GitURL == "" {
		plugin.GitURL = gitutil.FirstRemoteURL(plugin.LocalDir)
	}
	plugin.LocalCommit = gitutil.ShortHead(plugin.LocalDir)
	plugin.Branch = gitutil.Branch(plugin.LocalDir)
	plugin.Upstream = gitutil.Upstream(plugin.LocalDir)
	plugin.Dirty = gitutil.DirtyCount(plugin.LocalDir)
	if plugin.Upstream != "" {
		plugin.Ahead, plugin.Behind, _ = gitutil.AheadBehind(plugin.LocalDir, plugin.Upstream)
	}
}

package cli

import "github.com/cinvymoe/dshpm/internal/profile"

func loadProfile(dshHome, name string) (*profileProfile, error) {
	loaded, err := profile.Load(dshHome, name)
	if err != nil {
		return nil, err
	}
	return &profileProfile{Dependencies: loaded.Dependencies}, nil
}

package main

import "runtime/debug"

func cliVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			if len(setting.Value) > 12 {
				return setting.Value[:12]
			}
			return setting.Value
		}
	}
	return "dev"
}

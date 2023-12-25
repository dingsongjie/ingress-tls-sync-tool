package cmd

import (
	"os/exec"

	log "github.com/dingsongjie/file-help/pkg/log"
)

func Execute(cmds string) (bool, string) {
	cmd := exec.Command("bash", "-c", cmds)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Logger.Fatal("Command faild commands: " + cmds + " ")
		return false, string(output)
	}
	return true, string(output)
}

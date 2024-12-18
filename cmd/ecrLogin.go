package cmd

import (
	"log"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/ekini/ecs-tool/lib"
)

// ecrLoginCmd represents the ecrLogin command
var ecrLoginCmd = &cobra.Command{
	Use:   "ecr-login",
	Short: "Gets command for docker login",
	Long: `Gets command for docker login.

Use it like so:
$eval $(ecs-tool ecr-login)
`,
	Run: func(cmd *cobra.Command, args []string) {
		err := lib.EcrLogin(
			viper.GetString("profile"),
		)
		if err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	rootCmd.AddCommand(ecrLoginCmd)
}

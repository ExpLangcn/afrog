package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/urfave/cli/v2"
	"github.com/zan8in/afrog/internal/runner"
	"github.com/zan8in/afrog/pkg/config"
	"github.com/zan8in/afrog/pkg/core"
	"github.com/zan8in/afrog/pkg/html"
	"github.com/zan8in/afrog/pkg/log"
	"github.com/zan8in/afrog/pkg/poc"
	"github.com/zan8in/afrog/pkg/upgrade"
	"github.com/zan8in/afrog/pkg/utils"
)

var options = &config.Options{}
var htemplate = &html.HtmlTemplate{}
var lock sync.Mutex
var number = 0

func main() {
	app := cli.NewApp()
	app.Name = runner.ShowBanner()
	app.Usage = "v" + config.Version
	app.UsageText = runner.ShowUsage()
	app.Version = config.Version

	app.Flags = []cli.Flag{
		&cli.StringFlag{Name: "Target", Aliases: []string{"t"}, Destination: &options.Target, Value: "", Usage: "target URLs/hosts to scan"},
		&cli.StringFlag{Name: "TargetFilePath", Aliases: []string{"T"}, Destination: &options.TargetsFilePath, Value: "", Usage: "path to file containing a list of target URLs/hosts to scan (one per line)"},
		&cli.StringFlag{Name: "PocsFilePath", Aliases: []string{"P"}, Destination: &options.PocsFilePath, Value: "", Usage: "poc.yaml or poc directory paths to include in the scan（no default `afrog-pocs` directory）"},
		&cli.StringFlag{Name: "Output", Aliases: []string{"o"}, Destination: &options.Output, Value: "", Usage: "output html report, eg: -o result.html "},
		&cli.BoolFlag{Name: "Silent", Aliases: []string{"s"}, Destination: &options.Silent, Value: false, Usage: "no progress, only results"},
	}

	app.Action = func(c *cli.Context) error {
		upgrade := upgrade.New()
		upgrade.UpgradeAfrogPocs()

		runner.ShowBanner2(upgrade.LastestAfrogVersion)

		fmt.Println("PATH:")
		fmt.Println("   " + options.Config.GetConfigPath())
		fmt.Println("   " + poc.GetPocPath() + " v" + upgrade.LastestVersion)

		htemplate.Filename = options.Output
		if err := htemplate.New(); err != nil {
			return err
		}

		err := runner.New(options, func(result interface{}) {
			r := result.(*core.Result)

			lock.Lock()
			defer lock.Unlock()

			if !options.Silent {
				options.CurrentCount++
			}

			if r.IsVul {
				number++

				if len(r.Output) > 0 {
					htemplate.Result = r
					htemplate.Number = utils.GetNumberText(number)
					htemplate.Append()
				}

				r.PrintColorResultInfoConsole(utils.GetNumberText(number))
			}

			if !options.Silent {
				fmt.Printf("\r%d/%d | %d%% ", options.CurrentCount, options.Count, options.CurrentCount*100/options.Count)
			}

		})
		if err != nil {
			return err
		}

		return err
	}

	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(runner.ShowUsage())
		fmt.Println(log.LogColor.High("Failed to start afrog，", err.Error()))
	}
}

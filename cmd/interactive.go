package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/anmitsu/go-shlex"
	"github.com/chzyer/readline"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/natesales/pathvector/internal/process"
	"github.com/natesales/pathvector/pkg/config"
)

var (
	enable = false
	conf   *config.Config
	rline  *readline.Instance
)

var (
	errEnableRequired = errors.New("% Access denied (enable required)")
)

type nestedMapContainer struct {
	m map[string]interface{}
}

func completeType(c any, node *nestedMapContainer, target string) {
	if node == nil {
		node = &nestedMapContainer{m: map[string]interface{}{}}
	}
	if node.m == nil {
		node.m = map[string]interface{}{}
	}

	v := reflect.ValueOf(c)
	log.Tracef("Attempting to complete type %s", v.Type())
	for v.Kind() == reflect.Ptr { // Dereference pointer types
		v = v.Elem()
	}
	if target != "" {
		completeType(c, &nestedMapContainer{m: node.m[target].(map[string]interface{})}, "")
		return
	}
	vType := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := vType.Field(i)
		key := field.Tag.Get("yaml")
		description := field.Tag.Get("description")
		defaultValue := field.Tag.Get("default")
		if defaultValue == "-" {
			defaultValue = ""
		}

		if description == "" {
			log.Fatalf("%% Code error: %s in %s doesn't have a description: %+v", field.Name, vType.String(), c)
		} else if description != "-" { // Ignore descriptions that are -
			node.m[key] = map[string]interface{}{}
			if strings.Contains(field.Type.String(), "config.") { // If the type is a config struct
				if field.Type.Kind() == reflect.Map {
					newContainer := &nestedMapContainer{m: node.m[key].(map[string]interface{})}
					for _, k := range v.Field(i).MapKeys() {
						log.Tracef("Completing child struct type %s key %s[%s]", field.Type, key, k)
						kNoSpace := strings.ReplaceAll(k.String(), " ", `\ `)
						newContainer.m[kNoSpace] = map[string]interface{}{}
						completeType(v.Field(i).MapIndex(k).Interface(), newContainer, kNoSpace)
					}
				} else { // If not a map type, insert and recurse
					completeType(v.Field(i).Interface(), &nestedMapContainer{m: node.m[key].(map[string]interface{})}, "")
				}
			}
		}
	}
}

func getConfigValue(c any, namespace []string) (interface{}, error) {
	namespaceStr := "['" + strings.Join(namespace, `', '`) + `']`
	log.Debugln("Showing " + namespaceStr)

	if len(namespace) == 0 { // Global
		return c, nil
	}

	v := reflect.ValueOf(c)
	for v.Kind() == reflect.Ptr { // Dereference pointer types
		v = v.Elem()
	}

	if v.Kind() == reflect.Map {
		for _, k := range v.MapKeys() {
			if k.String() == namespace[0] {
				return getConfigValue(v.MapIndex(k).Interface(), namespace[1:])
			}
		}
		return nil, fmt.Errorf("%% Configuration item %+v not found map", namespaceStr)
	}
	vType := v.Type()
	for i := 0; i < v.NumField(); i++ {
		key := vType.Field(i).Tag.Get("yaml")
		value := v.Field(i).Interface()
		if namespace[0] == key {
			if len(namespace) == 1 { // Exact match
				return value, nil
			} else {
				return getConfigValue(value, namespace[1:])
			}
		}
	}

	return nil, fmt.Errorf("%% Configuration item '%+v' not found", namespace)
}

func setConfigValue(c any, namespace []string, targetValue string) {
	namespaceStr := "['" + strings.Join(namespace, `', '`) + `']`
	log.Debugf("Attempting to set '%s' to '%s'", namespaceStr, targetValue)

	v := reflect.ValueOf(c)
	for v.Kind() == reflect.Ptr { // Dereference pointer types
		v = v.Elem()
	}

	if v.Kind() == reflect.Map {
		for _, k := range v.MapKeys() {
			if k.String() == namespace[0] {
				log.Debugf("Found map element with key %s, recursing to set '%s' to %s", k.String(), namespace[1:], targetValue)
				setConfigValue(v.MapIndex(k).Interface(), namespace[1:], targetValue)
				return
			}
		}
		fmt.Printf("%% Configuration item %+v not found map", namespaceStr)
		return
	}

	vType := v.Type()
	log.Debugf("Iterating over type %s", vType)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		for f.Kind() == reflect.Ptr { // Dereference pointer types
			f = f.Elem()
		}
		key := vType.Field(i).Tag.Get("yaml")
		if namespace[0] == key {
			if len(namespace) > 1 {
				log.Debugf("Namespace still has more recursing to go, recursing with %s", namespace[1:])
				setConfigValue(f.Interface(), namespace[1:], targetValue)
			} else { // Exact match
				log.Debugf("Matched. Setting '%s' to '%s' with type %s", namespaceStr, targetValue, f.Kind())
				if f.IsValid() && f.CanSet() {
					switch f.Kind() {
					case reflect.Int, reflect.Int64:
						targetValAsInt, err := strconv.ParseInt(targetValue, 10, 64)
						if err != nil {
							fmt.Printf("%% Unable parse value '%s' as int: %s", targetValue, err)
							return
						}
						if !f.OverflowInt(targetValAsInt) {
							f.SetInt(targetValAsInt)
						}
					case reflect.Uint, reflect.Uint32:
						targetValAsUint, err := strconv.ParseUint(targetValue, 10, 64)
						if err != nil {
							fmt.Printf("%% Unable parse value '%s' as uint: %s", targetValue, err)
							return
						}
						if !f.OverflowUint(targetValAsUint) {
							f.SetUint(targetValAsUint)
						}
					case reflect.String:
						targetValAsString := fmt.Sprintf("%v", targetValue)
						f.SetString(targetValAsString)
					case reflect.Bool:
						if targetValue == "true" {
							f.SetBool(true)
						} else if targetValue == "false" {
							f.SetBool(false)
						} else {
							fmt.Printf("%% Unable parse value '%s' as bool", targetValue)
							return
						}
					default:
						fmt.Printf("%% Unable to set '%s' of type '%s'", namespaceStr, f.Kind())
						return
					}
				}
			}
		}
	}
}

func printTree(root *nestedMapContainer) {
	printTreeRec(root, 0)
}

// printTreeRec is the recursive function for printing the tree
func printTreeRec(node *nestedMapContainer, indent int) {
	for k, v := range node.m {
		val := v.(map[string]interface{})

		term := ";"
		if len(val) > 0 { // has children
			term = " {"
		}

		fmt.Printf("%s%s%s\n", strings.Repeat("  ", indent), k, term)
		printTreeRec(&nestedMapContainer{m: val}, indent+1)
		if term == " {" {
			fmt.Printf(strings.Repeat("  ", indent) + "}\n")
		}
	}
}

func prettyPrint(a any) {
	o, err := yaml.Marshal(a)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(strings.TrimSpace(string(o)))
}

// completeNode gets a list of prefix completer items for a given node
func completeNode(node *nestedMapContainer) []readline.PrefixCompleterInterface {
	var l []readline.PrefixCompleterInterface
	for k, v := range node.m {
		l = append(l, readline.PcItem(k, completeNode(&nestedMapContainer{m: v.(map[string]interface{})})...))
	}
	return l
}

func prompt(enable bool) string {
	suffix := "> "
	if enable {
		suffix = "# "
	}
	p := "pathvector " + suffix
	hostname, err := os.Hostname()
	if err == nil {
		p = "pathvector (" + hostname + ") " + suffix
	}
	return p
}

func runCommand(line string) {
	line = strings.TrimSpace(line)
	log.Tracef("Processing command '%s'", line)
	switch {
	case line == "enable":
		enable = true
		initRline()
		rline.SetPrompt(prompt(true))
	case line == "disable":
		enable = false
		initRline()
		rline.SetPrompt(prompt(false))
	case line == "show version":
		fmt.Printf("Pathvector version %s\n", version)
	case strings.HasPrefix(line, "show"):
		words, err := shlex.Split(strings.TrimPrefix(line, "show"), true)
		if err != nil {
			log.Fatal(err)
		}
		item, err := getConfigValue(&conf, words)
		if err != nil {
			fmt.Println(err)
		} else {
			prettyPrint(item)
		}
	case strings.HasPrefix(line, "set"):
		if !enable {
			fmt.Println(errEnableRequired)
			return
		}
		words, err := shlex.Split(strings.TrimPrefix(line, "set"), true)
		if err != nil {
			log.Fatal(err)
		}
		setConfigValue(&conf, words[:len(words)-1], words[len(words)-1])
	case line == "exit" || line == "quit":
		os.Exit(0)
	case line == "":
	default:
		fmt.Println("% Unknown command: " + line)
	}
}

func completer() *readline.PrefixCompleter {
	var root nestedMapContainer
	completeType(conf, &root, "")
	//printTree(&root)

	configCompletions := completeNode(&root)
	var c *readline.PrefixCompleter
	if enable {
		c = readline.NewPrefixCompleter(
			readline.PcItem("disable"),
			readline.PcItem("show", append(configCompletions, readline.PcItem("version"))...),
			readline.PcItem("set", configCompletions...),
			readline.PcItem("delete", configCompletions...),
			//readline.PcItem("create", topLevelCreate...) // TODO
		)
	} else {
		c = readline.NewPrefixCompleter(
			readline.PcItem("enable"),
			readline.PcItem("show", append(configCompletions, readline.PcItem("version"))...),
		)
	}
	return c
}

func initRline() {
	var err error
	rline, err = readline.NewEx(&readline.Config{
		Prompt:            prompt(enable),
		HistoryFile:       "/tmp/pathvector.cli-history",
		AutoComplete:      completer(),
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		log.Fatal(err)
	}
}

func init() {
	rootCmd.AddCommand(interactiveCmd)
}

var interactiveCmd = &cobra.Command{
	Use:   "interactive",
	Short: "Interactive CLI",
	Run: func(cmd *cobra.Command, args []string) {
		configFile, err := os.ReadFile(configFile)
		if err != nil {
			log.Fatal("Reading config file: " + err.Error())
		}
		conf, err = process.Load(configFile)
		if err != nil {
			log.Fatal(err)
		}

		initRline()
		defer rline.Close()
		log.SetOutput(rline.Stderr())

		if len(args) > 1 {
			enable = true
			runCommand(strings.Join(args[1:], " "))
		} else {
			for {
				line, err := rline.Readline()
				if err == readline.ErrInterrupt {
					if len(line) == 0 {
						break
					} else {
						continue
					}
				} else if err == io.EOF {
					break
				}

				runCommand(line)
			}
		}
	},
}

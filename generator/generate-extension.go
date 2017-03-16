// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/googleapis/gnostic/jsonschema"
	"github.com/googleapis/gnostic/printer"
)

var PROTO_OPTIONS_FOR_EXTENSION = []ProtoOption{
	ProtoOption{
		Name:  "java_multiple_files",
		Value: "true",
		Comment: "// This option lets the proto compiler generate Java code inside the package\n" +
			"// name (see below) instead of inside an outer class. It creates a simpler\n" +
			"// developer experience by reducing one-level of name nesting and be\n" +
			"// consistent with most programming languages that don't support outer classes.",
	},

	ProtoOption{
		Name:  "java_outer_classname",
		Value: "VendorExtensionProto",
		Comment: "// The Java outer classname should be the filename in UpperCamelCase. This\n" +
			"// class is only used to hold proto descriptor, so developers don't need to\n" +
			"// work with it directly.",
	},
}

const additionalCompilerCodeWithMain = "" +
	"func handleExtension(extensionName string, info yaml.MapSlice) (bool, proto.Message, error) {\n" +
	"      switch extensionName {\n" +
	"      // All supported extensions\n" +
	"      %s\n" +
	"      default:\n" +
	"        return false, nil, nil\n" +
	"       }\n" +
	"}\n" +
	"\n" +
	"func main() {\n" +
	"	openapiextension_v1.ProcessExtension(handleExtension)\n" +
	"}\n"

const caseString = "\n" +
	"case \"%s\":\n" +
	"newObject, err := %s.New%s(info, compiler.NewContext(\"$root\", nil))\n" +
	"return true, newObject, err"

func GenerateMainFile(packageName string, license string, codeBody string, imports []string) string {
	code := &printer.Code{}
	code.Print(license)
	code.Print("// THIS FILE IS AUTOMATICALLY GENERATED.\n")

	// generate package declaration
	code.Print("package %s\n", packageName)

	code.Print("import (")
	for _, filename := range imports {
		code.Print("\"" + filename + "\"")
	}
	code.Print(")\n")

	code.Print(codeBody)
	return code.String()
}

func getBaseFileNameWithoutExt(filePath string) string {
	tmp := filepath.Base(filePath)
	return tmp[0 : len(tmp)-len(filepath.Ext(tmp))]
}

func toProtoPackageName(input string) string {
	var out = ""
	nonAlphaNumeric := regexp.MustCompile("[^0-9A-Za-z_]+")
	input = nonAlphaNumeric.ReplaceAllString(input, "")
	for index, character := range input {
		if character >= 'A' && character <= 'Z' {
			if index > 0 && input[index-1] != '_' {
				out += "_"
			}
			out += string(character - 'A' + 'a')
		} else {
			out += string(character)
		}

	}
	return out
}

func GenerateExtension(schameFile string, outDir string) {

	outFileBaseName := getBaseFileNameWithoutExt(schameFile)
	extensionNameWithoutXDashPrefix := outFileBaseName[len("x-"):]
	outDir = path.Join(outDir, "openapi_extensions_"+extensionNameWithoutXDashPrefix)
	protoPackage := toProtoPackageName(extensionNameWithoutXDashPrefix)
	protoPackageName := strings.ToLower(protoPackage)
	goPackageName := protoPackageName

	protoOutDirectory := outDir + "/" + "proto"
	var err error

	baseSchema := jsonschema.NewSchemaFromFile("schema.json")
	baseSchema.ResolveRefs()
	baseSchema.ResolveAllOfs()

	openapiSchema := jsonschema.NewSchemaFromFile(schameFile)
	openapiSchema.ResolveRefs()
	openapiSchema.ResolveAllOfs()

	// build a simplified model of the types described by the schema
	cc := NewDomain(openapiSchema)

	// create a type for each object defined in the schema
	extensionToMessage := make(map[string]string)
	hasErrors := false
	if cc.Schema.Definitions != nil {
		for _, pair := range *(cc.Schema.Definitions) {
			definitionName := pair.Name
			definitionSchema := pair.Value
			// ensure the id field is set
			if definitionSchema.Id == nil || len(*(definitionSchema.Id)) == 0 {
				fmt.Printf("Schema for %s does not contain the required 'id' field. Value of the 'id' field should be the name of the OpenAPI extension that the schema represents\n", definitionName)
				hasErrors = true
			} else {
				if _, ok := extensionToMessage[*(definitionSchema.Id)]; ok {
					fmt.Printf("Schema %s and %s has the same 'id' field value\n", definitionName, extensionToMessage[*(definitionSchema.Id)])
					hasErrors = true
				}
				extensionToMessage[*(definitionSchema.Id)] = definitionName
			}
			typeName := cc.TypeNameForStub(definitionName)
			typeModel := cc.BuildTypeForDefinition(typeName, definitionName, definitionSchema)
			if typeModel != nil {
				cc.TypeModels[typeName] = typeModel
			}
		}
	}
	if hasErrors {
		// error has been reported.
		os.Exit(-1)
	}

	err = os.MkdirAll(outDir, os.ModePerm)
	if err != nil {
		panic(err)
	}

	err = os.MkdirAll(protoOutDirectory, os.ModePerm)
	if err != nil {
		panic(err)
	}

	// generate the protocol buffer description
	PROTO_OPTIONS = append(PROTO_OPTIONS_FOR_EXTENSION,
		ProtoOption{Name: "java_package", Value: "org.openapi.extension." + strings.ToLower(protoPackage), Comment: "// The Java package name must be proto package name with proper prefix."},
		ProtoOption{Name: "objc_class_prefix", Value: strings.ToLower(protoPackage),
			Comment: "// A reasonable prefix for the Objective-C symbols generated from the package.\n" +
				"// It should at a minimum be 3 characters long, all uppercase, and convention\n" +
				"// is to use an abbreviation of the package name. Something short, but\n" +
				"// hopefully unique enough to not conflict with things that may come along in\n" +
				"// the future. 'GPB' is reserved for the protocol buffer implementation itself.",
		})

	proto := cc.GenerateProto(protoPackageName, LICENSE, PROTO_OPTIONS_FOR_EXTENSION, nil)
	protoFilename := path.Join(protoOutDirectory, outFileBaseName+".proto")

	err = ioutil.WriteFile(protoFilename, []byte(proto), 0644)
	if err != nil {
		panic(err)
	}

	// generate the compiler
	compiler := cc.GenerateCompiler(goPackageName, LICENSE, []string{
		"fmt",
		"strings",
		"github.com/googleapis/gnostic/compiler",
	})
	goFilename := path.Join(protoOutDirectory, outFileBaseName+".go")
	err = ioutil.WriteFile(goFilename, []byte(compiler), 0644)
	if err != nil {
		panic(err)
	}
	err = exec.Command(runtime.GOROOT()+"/bin/gofmt", "-w", goFilename).Run()

	// generate the main file.
	outDirRelativeToGoPathSrc := strings.Replace(outDir, path.Join(os.Getenv("GOPATH"), "src")+"/", "", 1)
	imports := []string{
		"gopkg.in/yaml.v2",
		"github.com/golang/protobuf/proto",
		"github.com/googleapis/gnostic/extensions",
		"github.com/googleapis/gnostic/compiler",
		outDirRelativeToGoPathSrc + "/" + "proto",
	}
	var extensionNameKeys []string
	for k := range extensionToMessage {
		extensionNameKeys = append(extensionNameKeys, k)
	}
	sort.Strings(extensionNameKeys)

	var cases string
	for _, extensionName := range extensionNameKeys {
		cases += fmt.Sprintf(caseString, extensionName, goPackageName, extensionToMessage[extensionName])
	}
	extMainCode := fmt.Sprintf(additionalCompilerCodeWithMain, cases)

	main := GenerateMainFile("main", LICENSE, extMainCode, imports)
	mainFileName := path.Join(outDir, "main.go")
	err = ioutil.WriteFile(mainFileName, []byte(main), 0644)
	if err != nil {
		panic(err)
	}

	// format the compiler
	err = exec.Command(runtime.GOROOT()+"/bin/gofmt", "-w", mainFileName).Run()
}

func ProcessExtensionGenCommandline(usage string) {

	outDir := ""
	schameFile := ""
	goPathWithSrcDir := path.Join(os.Getenv("GOPATH"), "src")

	extParamRegex, _ := regexp.Compile("--(.+)=(.+)")

	for i, arg := range os.Args {
		if i == 0 {
			continue // skip the tool name
		}
		var m [][]byte
		if m = extParamRegex.FindSubmatch([]byte(arg)); m != nil {
			flagName := string(m[1])
			flagValue := string(m[2])
			switch flagName {
			case "out_dir":
				outDir = flagValue
			default:
				fmt.Printf("Unknown option: %s.\n%s\n", arg, usage)
				os.Exit(-1)
			}
		} else if arg == "--extension" {
			continue
		} else if arg[0] == '-' {
			fmt.Printf("Unknown option: %s.\n%s\n", arg, usage)
			os.Exit(-1)
		} else {
			schameFile = arg
		}
	}

	if schameFile == "" {
		fmt.Printf("No input json schema specified.\n%s\n", usage)
		os.Exit(-1)
	}
	if outDir == "" {
		fmt.Printf("Missing output directive.\n%s\n", usage)
		os.Exit(-1)
	}
	if !strings.HasPrefix(getBaseFileNameWithoutExt(schameFile), "x-") {
		fmt.Printf("Schema file name has to start with 'x-'.\n%s\n", usage)
		os.Exit(-1)
	}
	if !strings.HasPrefix(outDir, goPathWithSrcDir) {
		fmt.Printf("--out_dir should be inside $GOPATH '%s'.\n%s\n", goPathWithSrcDir, usage)
		os.Exit(-1)
	}

	GenerateExtension(schameFile, outDir)
}
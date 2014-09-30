package cli

import (
	"fmt"
	"log"
	"os"
	"regexp"

	"github.com/wanelo/image-server/core"
	"github.com/wanelo/image-server/parser"
	"github.com/wanelo/image-server/processor"

	"github.com/wanelo/image-server/uploader"
	mantaclient "github.com/wanelo/image-server/uploader/manta/client"
)

var pathHashRegex *regexp.Regexp

func init() {
	pathHashRegex = regexp.MustCompile(`\/([0-9a-f]{3})\/([0-9a-f]{3})\/([0-9a-f]{3})\/([0-9a-f]{23})\/`)
}

type ImageUpload struct {
	ServerConfiguration *core.ServerConfiguration
	Namespace           string
	Hash                string
	Filename            string
	LocalPath           string
	ContentType         string
}

func (iu *ImageUpload) Upload() error {
	uploader := uploader.DefaultUploader(iu.ServerConfiguration)
	remoteResizedPath := iu.ServerConfiguration.Adapters.Paths.RemoteImagePath(iu.Namespace, iu.Hash, iu.Filename)
	log.Printf("uploading %s to manta: %s", iu.LocalPath, remoteResizedPath)
	err := uploader.Upload(iu.LocalPath, remoteResizedPath, iu.ContentType)
	if err != nil {
		log.Println(err)
	}
	return nil
}

type ImageProcessor struct {
	Image     *Image
	Outputs   []string
	Namespace string
	channel   chan (string)
}

func NewImageProcessor(namespace string, path string, outputs []string) *ImageProcessor {
	processingChannel := make(chan string)
	return &ImageProcessor{
		Image:     NewImage(path, outputs, processingChannel),
		Outputs:   outputs,
		Namespace: namespace,
		channel:   processingChannel,
	}
}

func (ip *ImageProcessor) calculateMissingOutputs(sc *core.ServerConfiguration) ([]string, map[string]mantaclient.Entry, error) {
	// Determine what versions need to be generated
	var itemOutputs []string
	c := mantaclient.DefaultClient()
	c.HTTPTimeout = sc.HTTPTimeout
	m := make(map[string]mantaclient.Entry)
	remoteDirectory := sc.Adapters.Paths.RemoteImageDirectory(ip.Namespace, ip.Image.Hash)
	entries, err := c.ListDirectory(remoteDirectory)
	if err == nil {

		for _, entry := range entries {
			if entry.Type == "object" {
				m[entry.Name] = entry
			} else {
				// got a directory
			}
		}

		for _, output := range ip.Outputs {
			if _, ok := m[output]; ok {
				log.Printf("Skipping %s/%s", remoteDirectory, output)
			} else {
				itemOutputs = append(itemOutputs, output)
			}
		}

	} else {
		return nil, nil, err
	}

	return itemOutputs, m, nil
}

// ProcessMissing processes images missing in the remote server
func (ip *ImageProcessor) ProcessMissing(sc *core.ServerConfiguration) error {
	missingOutputs, _, err := ip.calculateMissingOutputs(sc)
	if err != nil {
		return err
	}

	for _, filename := range missingOutputs {
		err := ip.ProcessOutput(sc, filename)
		if err != nil {
			return err
		}
	}
	return nil
}

func (ip *ImageProcessor) ProcessOutput(sc *core.ServerConfiguration, filename string) error {
	go func() {
		err := ip.Image.ProcessOutput(sc, ip.Namespace, filename)
		if err != nil {
			log.Println("Something happened", err)
			os.Exit(1)
		}
	}()

	// when Image.ProcessOutput puts something on the channel, take that info
	// and run it through ImageUpload to get it into manta
	select {
	case localImagePath := <-ip.channel:
		// Hash                string

		upload := ImageUpload{
			ServerConfiguration: sc,
			LocalPath:           localImagePath,
			Filename:            filename,
			Namespace:           ip.Namespace,
			Hash:                ip.Image.Hash,
		}
		upload.Upload()
	}

	return nil
}

type Image struct {
	LocalOriginalPath string
	Outputs           []string
	Hash              string
	processingChannel chan string
}

func NewImage(path string, outputs []string, c chan string) *Image {
	img := &Image{
		LocalOriginalPath: path,
		Outputs:           outputs,
		processingChannel: c,
	}
	img.Hash = img.ToHash()
	return img
}

func (i *Image) ToHash() string {
	m := pathHashRegex.FindStringSubmatch(i.LocalOriginalPath)
	return fmt.Sprintf("%s%s%s%s", m[1], m[2], m[3], m[4])
}

// ProcessOutput
// Takes a filename, sends Image metadata through a processor to generate that new file
// Once complete, pushes a LocalImage onto channel c
func (i *Image) ProcessOutput(sc *core.ServerConfiguration, namespace string, filename string) error {
	ic, err := parser.NameToConfiguration(sc, filename)
	if err != nil {
		return fmt.Errorf("Error parsing name: %v\n", err)
	}

	ic.Namespace = namespace
	ic.ID = i.Hash

	pchan := &processor.ProcessorChannels{
		ImageProcessed: make(chan *core.ImageConfiguration),
		Skipped:        make(chan string),
	}

	localPath := sc.Adapters.Paths.LocalImagePath(namespace, i.Hash, filename)

	p := processor.Processor{
		Source:             i.LocalOriginalPath,
		Destination:        localPath,
		ImageConfiguration: ic,
		Channels:           pchan,
	}

	err = p.CreateImage()

	if err != nil {
		return err
	}

	select {
	case <-pchan.ImageProcessed:
		i.processingChannel <- localPath
	case path := <-pchan.Skipped:
		log.Println("Skipped processing (image)", path)
		i.processingChannel <- localPath
	}

	return nil
}

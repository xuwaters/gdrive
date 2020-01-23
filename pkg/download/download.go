package download

import (
	"bufio"
	"container/list"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type downloadConfig struct {
	CredFile  string `mapstructure:"cred_file"`
	TokenFile string `mapstructure:"token_file"`
	Src       string `mapstructure:"src"`
	Dst       string `mapstructure:"dst"`
}

func loadConfig() (*downloadConfig, error) {
	err := viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("load config failure: %w", err)
		}
	}
	config := &downloadConfig{}
	err = viper.Unmarshal(config)
	if err != nil {
		return nil, fmt.Errorf("Unable to unmarshal config, err = %w", err)
	}
	log.Printf("config loaded: %#v, settings = %#v", config, viper.AllSettings())
	return config, nil
}

func GetCmd() *cobra.Command {

	cmd := &cobra.Command{
		Use: "download",
		Run: func(cmd *cobra.Command, args []string) {
			config, err := loadConfig()
			if err != nil {
				log.Fatalf("load config failure, err = %#v", err)
				return
			}
			onRunDownload(cmd, args, config)
		},
	}

	flags := cmd.Flags()
	flags.String("cred-file", "", "credentials.json file for Google Drive API from gcloud console \nhttps://console.developers.google.com/apis/library/drive.googleapis.com")
	flags.String("src", "", "Source fileId in google drive")
	flags.String("dst", "", "Destination directory")
	flags.String("token-file", "", "token file that stores access and refresh tokens, and is created automatically")

	_ = cmd.MarkFlagRequired("src")
	_ = cmd.MarkFlagRequired("dst")

	err := viper.BindPFlags(flags)
	if err != nil {
		log.Fatalf("Unable to bind viper flags, err = %v", err)
	}

	// config file
	viper.SetConfigName("config-download")
	viper.AddConfigPath(".")
	viper.AddConfigPath(filepath.Base(os.Args[0]))

	// environment variables
	viper.AutomaticEnv()
	viper.SetEnvPrefix("gd")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "__", "-", "_"))

	return cmd
}

type Task struct {
	FileId   string
	SavePath string
}

func onRunDownload(cmd *cobra.Command, args []string, config *downloadConfig) {
	credBuffer, err := ioutil.ReadFile(config.CredFile)
	if err != nil {
		log.Printf("Unable to read client credentials file: %s, err = %v", config.CredFile, err)
		return
	}
	oauth2Config, err := google.ConfigFromJSON(credBuffer, drive.DriveReadonlyScope)
	if err != nil {
		log.Printf("Unable to parse credentials file to config, err = %v", err)
		return
	}

	ctx := context.Background()
	tokenSource := mustLoadTokenSource(ctx, oauth2Config, config.TokenFile)
	service, err := drive.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		log.Printf("Unable to create google drive service, err = %v", err)
		return
	}

	// queue of fileId or folderId
	taskQueue := list.New()
	taskQueue.PushBack(Task{
		FileId:   config.Src,
		SavePath: config.Dst,
	})

	for taskQueue.Len() != 0 {
		frontElem := taskQueue.Front()
		taskQueue.Remove(frontElem)
		task := frontElem.Value.(Task)
		// get meta data
		driveFile := service.Files.Get(task.FileId)
		currFile, err := driveFile.Fields("id,name,mimeType,md5Checksum").Do()
		if err != nil {
			log.Printf("get fileId: %s, err = %v", task.FileId, err)
			return
		}
		if isDriveFolder(currFile) {
			// list folder contents
			log.Printf(">> list folder: %s [%s]", currFile.Id, currFile.Name)
			err = listDriveFolder(service, currFile.Id, func(nextFile *drive.File) error {
				dstFilePath := filepath.Join(task.SavePath, nextFile.Name)
				if isDriveFolder(nextFile) {
					taskQueue.PushBack(Task{
						FileId:   nextFile.Id,
						SavePath: dstFilePath,
					})
					return nil
				}
				return downloadDriveFile(service, nextFile, dstFilePath)
			})
			if err != nil {
				log.Printf("list drive folder: %s [%s], err = %v", currFile.Id, currFile.Name, err)
				return
			}
		} else {
			// download file
			dstFilePath := filepath.Join(task.SavePath, currFile.Name)
			err = downloadDriveFile(service, currFile, dstFilePath)
			if err != nil {
				log.Printf("download drive file err = %v", err)
				return
			}
		}
	}
}

func listDriveFolder(service *drive.Service, folderId string, handler func(*drive.File) error) error {
	pageToken := ""
	for i := 0; ; i++ {
		log.Printf("[%03d] listing folder: %s", i, folderId)
		resp, err := service.Files.List().
			PageSize(10).
			PageToken(pageToken).
			Spaces("drive").
			Corpora("user").
			Q(fmt.Sprintf("'%s' in parents", folderId)).
			Fields("nextPageToken, files(id,name,mimeType,md5Checksum)").
			Do()
		if err != nil {
			return fmt.Errorf("Unable to list files, err = %w", err)
		}
		if len(resp.Files) == 0 {
			log.Printf("No files found in folder: %s", folderId)
			return nil
		}
		for _, entry := range resp.Files {
			if err := handler(entry); err != nil {
				return err
			}
		}
		log.Printf("[%03d] nextPageToken = %s", i, resp.NextPageToken)
		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return nil
}

func downloadDriveFile(service *drive.Service, currFile *drive.File, dstFilePath string) error {
	driveFile := service.Files.Get(currFile.Id)
	// check md5
	dstFileMd5 := getFileMd5(dstFilePath)
	if dstFileMd5 != "" && dstFileMd5 == currFile.Md5Checksum {
		log.Printf("skipping identical file: %s", dstFilePath)
		return nil
	}
	log.Printf("downloading file: %s", dstFilePath)
	resp, err := driveFile.Download()
	if err != nil {
		return fmt.Errorf("download file: %s, err = %w", dstFilePath, err)
	}
	err = saveFile(dstFilePath, resp.Body)
	if err != nil {
		return fmt.Errorf("save file: %s, err = %w", dstFilePath, err)
	}
	return nil
}

func isDriveFolder(driveFile *drive.File) bool {
	return strings.HasSuffix(driveFile.MimeType, "folder")
}

func getFileMd5(dstFilePath string) string {
	checksum := md5.New()
	fin, err := os.Open(dstFilePath)
	if err != nil {
		return ""
	}
	defer fin.Close()

	buff := make([]byte, 64*1024)
	for {
		n, err := fin.Read(buff)
		if err == io.EOF {
			break
		}
		_, _ = checksum.Write(buff[:n])
	}
	return hex.EncodeToString(checksum.Sum(nil))
}

func saveFile(dstFilePath string, reader io.ReadCloser) error {
	defer reader.Close()

	_ = os.MkdirAll(filepath.Dir(dstFilePath), 0755)

	fout, err := os.Create(dstFilePath)
	if err != nil {
		return fmt.Errorf("Unable to create file: %s, err = %w", dstFilePath, err)
	}
	defer fout.Close()

	writer := bufio.NewWriterSize(fout, 1024 * 1024)
	defer writer.Flush()

	_, err = io.Copy(writer, reader)
	if err != nil {
		return fmt.Errorf("Unable to write file: %s, err = %w", dstFilePath, err)
	}
	return nil
}

func mustLoadTokenSource(ctx context.Context, config *oauth2.Config, tokenFile string) oauth2.TokenSource {
	// read token file
	token, err := loadTokenFromFile(tokenFile)
	if err != nil {
		token, err = loadTokenFromWeb(config, 10*time.Second)
		if err != nil {
			log.Fatalf("load token err = %v", err)
		}
		if err = saveToken(tokenFile, token); err != nil {
			log.Fatalf("save token err = %v", err)
		}
	}
	log.Printf("token loaded: %s", token.TokenType)
	return config.TokenSource(ctx, token)
}

func saveToken(tokenFile string, token *oauth2.Token) error {
	fout, err := os.Create(tokenFile)
	if err != nil {
		return fmt.Errorf("Save token to file: %s, err = %w", tokenFile, err)
	}
	defer fout.Close()
	err = json.NewEncoder(fout).Encode(token)
	if err != nil {
		return fmt.Errorf("Encode token to file: %s, err = %w", tokenFile, err)
	}
	return nil
}

func loadTokenFromFile(tokenFile string) (*oauth2.Token, error) {
	fin, err := os.Open(tokenFile)
	if err != nil {
		return nil, err
	}
	defer fin.Close()

	token := &oauth2.Token{}
	err = json.NewDecoder(fin).Decode(token)
	return token, err
}

func loadTokenFromWeb(config *oauth2.Config, timeout time.Duration) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Open the following link in browser:\n %s\n", authURL)
	fmt.Printf("Then type the authorization code: ")

	authCode := ""
	if _, err := fmt.Scan(&authCode); err != nil {
		return nil, fmt.Errorf("Unable to read authorization code: %w", err)
	}
	ctx, ctxCancel := context.WithTimeout(context.Background(), timeout)
	defer ctxCancel()
	token, err := config.Exchange(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve token from web: %w", err)
	}
	return token, nil
}

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
	ListFile  string `mapstructure:"list_file"` // save file list meta
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
	flags.String("cred_file", "", "credentials.json file for Google Drive API from gcloud console \nhttps://console.developers.google.com/apis/library/drive.googleapis.com")
	flags.String("src", "", "Source fileId in google drive")
	flags.String("dst", "", "Destination directory")
	flags.String("token_file", "", "token file that stores access and refresh tokens, and is created automatically")
	flags.String("list_file", "", "list of files to be downloaded, will be created automatically")

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
	FileId      string `json:"id"`
	SavePath    string `json:"path"`
	Md5Checksum string `json:"md5"`
	Done        bool   `json:"done"`
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

	root := Task{
		FileId:   config.Src,
		SavePath: config.Dst,
	}

	var fileTasks []Task

	fileTasks, err = loadListFile(config.ListFile)
	if err != nil {
		log.Printf("Load list file err = %v", err)
		fileTasks, err = listDriveFolderFiles(service, root)
		if err != nil {
			log.Printf("list files: err = %v", err)
			return
		}
		// save lists
		_ = saveListFile(fileTasks, config.ListFile)
	}

	total := len(fileTasks)
	log.Printf("Total files: %d", total)

	for i, task := range fileTasks {
		if task.Done {
			log.Printf("Skipping: %05d / %05d, file: %s", i, total, task.SavePath)
			continue
		}
		percent := float64(i) * 100.0 / float64(total)
		log.Printf("Downloading: %05d / %05d (%.2f %%)", i, total, percent)
		for k := 1; k <= 5; k++ {
			err = downloadDriveFile(service, task)
			if err == nil {
				break
			}
			sleepDuration := time.Duration(k*5) * time.Second
			log.Printf("retry [%02d] in %v, err = %v", k, sleepDuration, err)
			time.Sleep(sleepDuration)
		}
		if err != nil {
			log.Printf("download err = %v", err)
			break
		}
		fileTasks[i].Done = true

		// save list file periodically
		if (i+1)%10 == 0 {
			_ = saveListFile(fileTasks, config.ListFile)
		}
	}

	// save list file
	_ = saveListFile(fileTasks, config.ListFile)
}

func loadListFile(listFile string) ([]Task, error) {
	fin, err := os.Open(listFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to open list file: %s, err = %w", listFile, err)
	}
	defer fin.Close()
	var tasks []Task
	decoder := json.NewDecoder(fin)
	err = decoder.Decode(&tasks)
	if err != nil {
		return nil, fmt.Errorf("Unable to decode list file: %s, err = %w", listFile, err)
	}
	return tasks, nil
}

func saveListFile(fileTasks []Task, listFile string) error {
	fout, err := os.Create(listFile)
	if err != nil {
		return fmt.Errorf("Unable create list file: %s, err = %w", listFile, err)
	}
	defer fout.Close()

	encoder := json.NewEncoder(fout)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(fileTasks)
	if err != nil {
		return fmt.Errorf("Marshal tasks err = %w", err)
	}

	return nil
}

func listDriveFolderFiles(service *drive.Service, rootFolder Task) ([]Task, error) {
	fileTasks := []Task{}
	taskQueue := list.New()
	taskQueue.PushBack(rootFolder)
	for taskQueue.Len() != 0 {
		frontElem := taskQueue.Front()
		taskQueue.Remove(frontElem)
		task := frontElem.Value.(Task)
		// get meta data
		driveFile := service.Files.Get(task.FileId)
		currFile, err := driveFile.Fields("id,name,mimeType,md5Checksum").Do()
		if err != nil {
			return nil, fmt.Errorf("get fileId: %s, err = %v", task.FileId, err)
		}
		if isDriveFolder(currFile) {
			// list folder contents
			log.Printf(">> list folder: %s [%s]", currFile.Id, currFile.Name)
			err = listDriveFolder(service, currFile.Id, func(nextFile *drive.File) error {
				dstFilePath := filepath.Join(task.SavePath, nextFile.Name)
				nextTask := Task{
					FileId:      nextFile.Id,
					SavePath:    dstFilePath,
					Md5Checksum: nextFile.Md5Checksum,
				}
				if isDriveFolder(nextFile) {
					taskQueue.PushBack(nextTask)
				} else {
					fileTasks = append(fileTasks, nextTask)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("list drive folder: %s [%s], err = %w", currFile.Id, currFile.Name, err)
			}
		} else {
			// download file
			dstFilePath := filepath.Join(task.SavePath, currFile.Name)
			fileTasks = append(fileTasks, Task{
				FileId:      currFile.Id,
				SavePath:    dstFilePath,
				Md5Checksum: currFile.Md5Checksum,
			})
		}
	}
	return fileTasks, nil
}

func listDriveFolder(service *drive.Service, folderId string, handler func(*drive.File) error) error {
	pageToken := ""
	for i := 0; ; i++ {
		log.Printf("[%03d] listing folder: %s", i, folderId)
		resp, err := service.Files.List().
			PageSize(100).
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
		pageToken = resp.NextPageToken
		if pageToken == "" {
			log.Printf("[%03d] finished", i)
			break
		}
		log.Printf("[%03d] nextPageToken = %s", i, resp.NextPageToken)
	}
	return nil
}

func downloadDriveFile(service *drive.Service, task Task) error {
	driveFile := service.Files.Get(task.FileId)
	// check md5
	dstFilePath := task.SavePath
	dstFileMd5 := getFileMd5(dstFilePath)
	if task.Md5Checksum != "" && dstFileMd5 != "" && dstFileMd5 == task.Md5Checksum {
		log.Printf("skipping identical file: %s", dstFilePath)
		return nil
	}
	log.Printf("downloading file (%s): %s", task.Md5Checksum, dstFilePath)
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

	writer := bufio.NewWriterSize(fout, 1024*1024)
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

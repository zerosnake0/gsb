package main

import (
	"archive/zip"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/json-iterator/go"

	"gsb/asset"
	"gsb/config"
)

const (
	cfgFileName   = "config.json"
	zipNameFormat = "20060102_150405.zip"
)

var (
	port        int
	rootSaveDir string
)

func init() {
	flag.IntVar(&port, "port", 9000, "listen port")
	flag.StringVar(&rootSaveDir, "root", config.DefaultRoot, "root for save")
	flag.Parse()
}

func loadTemplate() (*template.Template, error) {
	var err error
	t := template.New("")
	for name, file := range asset.Assets.Files {
		if file.IsDir() || !strings.HasSuffix(name, ".html") {
			continue
		}
		t, err = t.New(name).Parse(string(file.Data))
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

type saveConfig struct {
	Src string `json:"src"`
}

func getConfig(name string) (*saveConfig, error) {
	cfgFilePath := filepath.Join(rootSaveDir, name, cfgFileName)
	fp, err := os.Open(cfgFilePath)
	if err != nil {
		return nil, err
	}
	defer fp.Close()
	cfg := new(saveConfig)
	if err := jsoniter.NewDecoder(fp).Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func getAllSaves(name string) ([]string, error) {
	path := filepath.Join(rootSaveDir, name)
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}
	saves := make([]string, 0, len(files))
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		fname := file.Name()
		_, err := time.ParseInLocation(zipNameFormat, fname, time.UTC)
		if err != nil {
			continue
		}
		saves = append(saves, fname)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(saves)))
	return saves, nil
}

const (
	createFileFlags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
)

func createFile(path string) (*os.File, error) {
	return os.OpenFile(path, createFileFlags, 0644)
}

func zipIntoFp(src string, fp *os.File) error {
	zw := zip.NewWriter(fp)
	defer zw.Close()

	root := filepath.Dir(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		log.Printf("> %s", path)
		if err != nil {
			return err
		}
		fh, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		fh.Method = zip.Deflate
		fh.Name, err = filepath.Rel(root, path)
		if err != nil {
			return err
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		if filepath.Separator == '\\' {
			fh.Name = strings.ReplaceAll(fh.Name, "\\", "/")
		}
		if info.IsDir() {
			if !strings.HasSuffix(fh.Name, "/") {
				fh.Name += "/"
			}
			return nil
		}
		fp, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fp.Close()
		_, err = io.Copy(w, fp)
		return err
	})
}

func backupToZip(src, tgt string) error {
	log.Printf("~ %s -> %s", src, tgt)
	fp, err := createFile(tgt)
	if err != nil {
		return err
	}
	defer fp.Close()
	return zipIntoFp(src, fp)
}

func recoverFromZip(src, tgt string) error {
	log.Printf("~ %s <- %s", tgt, src)
	var tmpPath string
	_, err := os.Stat(src)
	if err != nil {
		tmpPath = tgt + ".bkup.zip"
		_, err := os.Stat(tmpPath)
		if err == nil {
			return fmt.Errorf("please remove %s", tmpPath)
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := backupToZip(tgt, tmpPath); err != nil {
			log.Printf("unable to backup for recover: %v", err)
			return err
		}
	}
	return func() (err error) {
		defer func() {
			if err == nil {
				if tmpPath != "" {
					if err = os.Remove(tmpPath); err != nil {
						log.Printf("unable to cleanup: %v", err)
					}
				}
			}
		}()
		if err := os.RemoveAll(tgt); err != nil {
			log.Printf("unable to remove target: %v", err)
			return err
		}
		root := filepath.Dir(tgt)
		fp, err := zip.OpenReader(src)
		if err != nil {
			log.Printf("unable to open zip: %v", err)
			return err
		}
		defer fp.Close()
		for _, f := range fp.File {
			path := filepath.Join(root, f.Name)
			info := f.FileInfo()
			if info.IsDir() {
				log.Printf("+ %s ...", path)
				if err := os.MkdirAll(path, info.Mode()); err != nil {
					log.Printf("unable to mkdir: %v", err)
					return err
				}
				continue
			}
			log.Printf("< %s ...", path)
			rc, err := f.Open()
			if err != nil {
				log.Printf("unable to open file in zip: %v", err)
				return err
			}
			if err := func() error {
				defer rc.Close()
				fp, err := os.OpenFile(path, createFileFlags, info.Mode())
				if err != nil {
					log.Printf("unable to create file: %v", err)
					return err
				}
				defer fp.Close()
				_, err = io.Copy(fp, rc)
				return err
			}(); err != nil {
				return err
			}
		}
		return nil
	}()
}

func main() {
	t, err := loadTemplate()
	if err != nil {
		panic(err)
	}

	var deleteLock sync.Mutex
	var recoverLock sync.Mutex

	engine := gin.New()
	engine.SetHTMLTemplate(t)
	engine.Use(gin.Recovery())
	/*
	engine.Use(func(c *gin.Context) {
		b, err := httputil.DumpRequest(c.Request, true)
		if err != nil {
			panic(err)
		}
		log.Printf(string(b))
	})
	*/
	engine.GET("/static/*name", func(c *gin.Context) {
		name := c.Param("name")
		file, ok := asset.Assets.Files["/static"+name]
		if !ok {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		var contentType string
		if strings.HasPrefix(name, "/css/") {
			contentType = "text/css"
		}
		c.Data(http.StatusOK, contentType, file.Data)
	})

	engine.GET("/", func(c *gin.Context) {
		var p struct {
			Error string `form:"error"`
		}
		if err := c.ShouldBindQuery(&p); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}

		fps, err := ioutil.ReadDir(rootSaveDir)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		var games []string
		for _, fp := range fps {
			if !fp.IsDir() {
				continue
			}
			games = append(games, fp.Name())
		}
		c.HTML(http.StatusOK, "/static/html/index.html", gin.H{
			"error": p.Error,
			"games": games,
		})
	})

	engine.POST("/", func(c *gin.Context) {
		if err := func() error {
			var p struct {
				Name string `form:"name" binding:"required"`
				Path string `form:"path" binding:"required"`
			}
			if err := c.ShouldBind(&p); err != nil {
				return err
			}
			// target path
			tgtPath := filepath.Join(rootSaveDir, p.Name)
			if rootSaveDir != filepath.Dir(tgtPath) {
				return errors.New("bad name " + p.Name)
			}
			if _, err := os.Stat(tgtPath); err == nil || !os.IsNotExist(err) {
				return err
			}
			// src path
			if _, err = os.Stat(p.Path); err != nil {
				return err
			}

			// make config
			cfg := saveConfig{
				Src: p.Path,
			}
			b, err := jsoniter.Marshal(cfg)
			if err != nil {
				return err
			}

			// create directory
			if err := os.Mkdir(tgtPath, 0755); err != nil {
				return err
			}
			fp, err := createFile(filepath.Join(tgtPath, cfgFileName))
			if err != nil {
				return err
			}
			defer fp.Close()
			if _, err := fp.Write(b); err != nil {
				return err
			}
			c.Redirect(http.StatusMovedPermanently, "/game/"+p.Name)
			return nil
		}(); err != nil {
			v := url.Values{}
			v.Set("error", err.Error())
			c.Redirect(http.StatusMovedPermanently, "/?"+v.Encode())
			return
		}
	})

	engine.GET("/game/:name", func(c *gin.Context) {
		var p struct {
			Error string `form:"error"`
		}
		if err := c.ShouldBindQuery(&p); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}

		name := c.Param("name")
		saves, err := getAllSaves(name)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.HTML(http.StatusOK, "/static/html/game.html", gin.H{
			"error": p.Error,
			"name":  name,
			"saves": saves,
		})
	})

	engine.POST("/game/:name", func(c *gin.Context) {
		name := c.Param("name")
		v := url.Values{}
		if err := func() error {
			cfg, err := getConfig(name)
			if err != nil {
				return err
			}
			for i := 0; i < 3; i++ {
				tgt := filepath.Join(rootSaveDir, name, time.Now().UTC().Format(zipNameFormat))
				if _, err := os.Stat(tgt); os.IsNotExist(err) {
					return backupToZip(cfg.Src, tgt)
				}
				time.Sleep(time.Second)
			}
			return errors.New("failed to find a timestamp")
		}(); err != nil {
			v.Set("error", err.Error())
		}
		c.Redirect(http.StatusMovedPermanently, "/game/"+name+"?"+v.Encode())
	})

	engine.POST("/game/:name/delall", func(c *gin.Context) {
		name := c.Param("name")
		v := url.Values{}
		if err := func() error {
			deleteLock.Lock()
			defer deleteLock.Unlock()
			saves, err := getAllSaves(name)
			if err != nil {
				return err
			}
			if len(saves) <= 1 {
				return errors.New("no save to be deleted")
			}
			for idx, save := range saves {
				if idx > 0 {
					path := filepath.Join(rootSaveDir, name, save)
					log.Printf("- removing %s ...", path)
					if err := os.Remove(path); err != nil {
						return err
					}
				}
			}
			return nil
		}(); err != nil {
			v.Set("error", err.Error())
		}
		c.Redirect(http.StatusMovedPermanently, "/game/"+name+"?"+v.Encode())
	})

	gamesaveWrap := func(cb func(name, zipName string, cfg *saveConfig) error) gin.HandlerFunc {
		return func(c *gin.Context) {
			name := c.Param("name")
			zipName := c.Param("zip")
			v := url.Values{}
			if err := func() error {
				cfg, err := getConfig(name)
				if err != nil {
					return err
				}
				return cb(name, zipName, cfg)
			}(); err != nil {
				v.Set("error", err.Error())
			}
			c.Redirect(http.StatusMovedPermanently, "/game/"+name+"?"+v.Encode())
		}
	}
	engine.POST("/game/:name/rec/:zip", gamesaveWrap(
		func(name, zipName string, cfg *saveConfig) error {
			recoverLock.Lock()
			defer recoverLock.Unlock()
			return recoverFromZip(filepath.Join(rootSaveDir, name, zipName), cfg.Src)
		}))
	engine.POST("/game/:name/del/:zip", gamesaveWrap(
		func(name, zipName string, cfg *saveConfig) error {
			deleteLock.Lock()
			defer deleteLock.Unlock()
			saves, err := getAllSaves(name)
			if err != nil {
				return err
			}
			if len(saves) <= 1 {
				return errors.New("no save to be deleted")
			}
			if zipName == saves[0] {
				return errors.New("the first save cannot be deleted")
			}
			path := filepath.Join(rootSaveDir, name, zipName)
			log.Printf("- removing %s ...", path)
			return os.Remove(path)
		}))

	engine.Run("localhost:" + strconv.Itoa(port))
}

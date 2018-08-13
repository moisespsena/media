package media

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"reflect"

	"code.cloudfoundry.org/bytefmt"
	"github.com/dustin/go-humanize"
	"github.com/gosimple/slug"
	"github.com/jinzhu/gorm"
	"github.com/jinzhu/inflection"
	"github.com/qor/admin"
	"github.com/qor/qor"
	"github.com/qor/qor/resource"
	"github.com/qor/qor/utils"
)

// CropOption includes crop options
type CropOption struct {
	X, Y, Width, Height int
}

// FileHeader is an interface, for matched values, when call its `Open` method will return `multipart.File`
type FileHeader interface {
	Open() (multipart.File, error)
}

type fileWrapper struct {
	*os.File
}

func (fileWrapper *fileWrapper) Open() (multipart.File, error) {
	return fileWrapper.File, nil
}

// Base defined a base struct for storages
type Base struct {
	FileName    string
	Url         string
	CropOptions map[string]*CropOption `json:",omitempty"`
	Delete      bool                   `json:"-"`
	Crop        bool                   `json:"-"`
	FileHeader  FileHeader             `json:"-"`
	Reader      io.Reader              `json:"-"`
	Options     map[string]string      `json:",omitempty"`
	cropped     bool
}

func (b *Base) CallFieldScan(field *reflect.StructField, data interface{}, scan func(data interface{}) error) (err error) {
	tags := utils.ParseTagOption(field.Tag.Get("oss"))
	var (
		types    []string
		fileName string
		size     uint64
		maxSize  uint64
		check    []func()
	)

	if tag, ok := tags["TYPES"]; ok {
		for _, typ := range strings.Split(tag, ",") {
			typ = strings.TrimSpace(typ)
			if typ != "" {
				types = append(types, strings.ToLower(typ))
			}

			check = append(check, func() {
				ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))

				for _, typ := range types {
					if typ == ext {
						return
					}
				}
				err = fmt.Errorf("Invalid file type %q", ext)
			})
		}
	}

	if tag, ok := tags["MAX-SIZE"]; ok {
		maxSize, err = bytefmt.ToBytes(strings.TrimSpace(tag))
		if err != nil {
			return fmt.Errorf("Parse Field TAG MAX-SIZE: %v", err)
		}

		check = append(check, func() {
			if size > maxSize {
				err = fmt.Errorf("Very large file. The expected maximum size is %s, but obtained %s.",
					humanize.Bytes(maxSize), humanize.Bytes(size))
			}
		})
	}

	if len(types) > 0 || maxSize > 0 {
		switch values := data.(type) {
		case *os.File:
			fileName = values.Name()
			var stat os.FileInfo
			if stat, err = values.Stat(); err != nil {
				return
			}
			size = uint64(stat.Size())
		case *multipart.FileHeader:
			fileName = values.Filename
			size = uint64(values.Size)
		case []*multipart.FileHeader:
			if len(values) == 1 {
				if file := values[0]; file.Size > 0 {
					fileName = file.Filename
					size = uint64(file.Size)
				}
			} else if len(values) > 1 {
				for i, file := range values {
					if file.Size > 0 {
						fileName = file.Filename
						size = uint64(file.Size)
						for _, cb := range check {
							cb()
							if err != nil {
								return fmt.Errorf("File #%d: %v", i, err)
							}
						}
					}
				}
				check = []func(){}
			}
		}

		if fileName != "" && size > 0 {
			for _, cb := range check {
				cb()
				if err != nil {
					return err
				}
			}
		}
	}

	return scan(data)
}

func (b *Base) FieldScan(field *reflect.StructField, data interface{}) (err error) {
	return b.CallFieldScan(field, data, b.Scan)
}

// Scan scan files, crop options, db values into struct
func (b *Base) Scan(data interface{}) (err error) {
	switch values := data.(type) {
	case *os.File:
		b.FileHeader = &fileWrapper{values}
		b.FileName = filepath.Base(values.Name())
	case *multipart.FileHeader:
		b.FileHeader, b.FileName = values, values.Filename
	case []*multipart.FileHeader:
		if len(values) > 0 {
			if file := values[0]; file.Size > 0 {
				b.FileHeader, b.FileName = file, file.Filename
			}
		}
	case []byte:
		if string(values) != "" {
			if err = json.Unmarshal(values, b); err == nil {
				var options struct {
					Crop   bool
					Delete bool
				}
				if err = json.Unmarshal(values, &options); err == nil {
					if options.Crop {
						b.Crop = true
					}
					if options.Delete {
						b.Delete = true
					}
				}
			}
		}
	case string:
		return b.Scan([]byte(values))
	case []string:
		for _, str := range values {
			if err := b.Scan(str); err != nil {
				return err
			}
		}
	default:
		err = errors.New("unsupported driver -> Scan pair for MediaLibrary")
	}

	// If image is deleted, then clean up all values, for serialized fields
	if b.Delete {
		b.Url = ""
		b.FileName = ""
		b.CropOptions = nil
	}
	return
}

// Value return struct's Value
func (b Base) Value() (driver.Value, error) {
	if b.Delete {
		return nil, nil
	}

	results, err := json.Marshal(b)
	return string(results), err
}

func (b Base) Ext() string {
	return strings.ToLower(path.Ext(b.Url))
}

// URL return file's url with given style
func (b Base) URL(styles ...string) string {
	if b.Url != "" && len(styles) > 0 {
		ext := path.Ext(b.Url)
		return fmt.Sprintf("%v.%v%v", strings.TrimSuffix(b.Url, ext), styles[0], ext)
	}
	return b.Url
}

// String return file's url
func (b Base) String() string {
	return b.URL()
}

// GetFileName get file's name
func (b Base) GetFileName() string {
	if b.FileName != "" {
		return b.FileName
	}
	if b.Url != "" {
		return filepath.Base(b.Url)
	}
	return ""
}

// GetFileHeader get file's header, this value only exists when saving files
func (b Base) GetFileHeader() FileHeader {
	return b.FileHeader
}

// GetURLTemplate get url template
func (b Base) GetURLTemplate(option *Option) (path string) {
	if path = option.Get("URL"); path == "" {
		path = "/system/{{class}}/{{primary_key}}/{{column}}/{{filename_with_hash}}"
	}
	return
}

var urlReplacer = regexp.MustCompile("(\\s|\\+)+")

func getFuncMap(scope *gorm.Scope, field *gorm.Field, filename string) template.FuncMap {
	hash := func() string { return strings.Replace(time.Now().Format("20060102150506.000000000"), ".", "", -1) }
	return template.FuncMap{
		"class":       func() string { return inflection.Plural(utils.ToParamString(scope.GetModelStruct().ModelType.Name())) },
		"primary_key": func() string { return fmt.Sprintf("%v", scope.PrimaryKeyValue()) },
		"column":      func() string { return strings.ToLower(field.Name) },
		"filename":    func() string { return filename },
		"basename":    func() string { return strings.TrimSuffix(path.Base(filename), path.Ext(filename)) },
		"hash":        hash,
		"filename_with_hash": func() string {
			return urlReplacer.ReplaceAllString(fmt.Sprintf("%s.%v%v", slug.Make(strings.TrimSuffix(path.Base(filename), path.Ext(filename))), hash(), path.Ext(filename)), "-")
		},
		"extension": func() string { return strings.TrimPrefix(path.Ext(filename), ".") },
	}
}

// GetURL get default URL for a model based on its options
func (b Base) GetURL(option *Option, scope *gorm.Scope, field *gorm.Field, templater URLTemplater) string {
	if path := templater.GetURLTemplate(option); path != "" {
		tmpl := template.New("").Funcs(getFuncMap(scope, field, b.GetFileName()))
		if tmpl, err := tmpl.Parse(path); err == nil {
			var result = bytes.NewBufferString("")
			if err := tmpl.Execute(result, scope.Value); err == nil {
				return result.String()
			}
		}
	}
	return ""
}

// Cropped mark the image to be cropped
func (b *Base) Cropped(values ...bool) (result bool) {
	result = b.cropped
	for _, value := range values {
		b.cropped = value
	}
	return result
}

// NeedCrop return the file needs to be cropped or not
func (b *Base) NeedCrop() bool {
	return b.Crop
}

// GetCropOption get crop options
func (b *Base) GetCropOption(name string) *image.Rectangle {
	if cropOption := b.CropOptions[strings.Split(name, "@")[0]]; cropOption != nil {
		return &image.Rectangle{
			Min: image.Point{X: cropOption.X, Y: cropOption.Y},
			Max: image.Point{X: cropOption.X + cropOption.Width, Y: cropOption.Y + cropOption.Height},
		}
	}
	return nil
}

// Retrieve retrieve file content with url
func (b Base) Retrieve(url string) (*os.File, error) {
	return nil, errors.New("not implemented")
}

// GetSizes get configured sizes, it will be used to crop images accordingly
func (b Base) GetSizes() map[string]*Size {
	return map[string]*Size{}
}

// IsImage return if it is an image
func (b Base) IsImage() bool {
	return IsImageFormat(b.URL())
}

func (b Base) IsVideo() bool {
	return IsVideoFormat(b.URL())
}

func (b Base) IsSVG() bool {
	return IsSVGFormat(b.URL())
}

func init() {
	admin.RegisterViewPath("github.com/qor/media/views")
}

// ConfigureQorMetaBeforeInitialize configure this field for Qor Admin
func (Base) ConfigureQorMetaBeforeInitialize(meta resource.Metaor) {
	if meta, ok := meta.(*admin.Meta); ok {
		if meta.Type == "" {
			meta.Type = "file"
		}

		if meta.GetFormattedValuer() == nil {
			meta.SetFormattedValuer(func(value interface{}, context *qor.Context) interface{} {
				return utils.Stringify(meta.GetValuer()(value, context))
			})
		}
	}
}

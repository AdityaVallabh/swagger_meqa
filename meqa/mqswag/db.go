package mqswag

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AdityaVallabh/swagger_meqa/meqa/mqutil"

	spec "github.com/getkin/kin-openapi/openapi3"
	"github.com/xeipuuv/gojsonschema"
)

// This file implements the in-memory DB that holds all the entity objects.

// Schema is the swagger spec schema.
type SchemaRef spec.SchemaRef
type Schema spec.Schema

// Returns all the first level property names for this schema. We will follow the $refs until
// we hit a map.
func (schema SchemaRef) GetProperties(swagger *Swagger) map[string]*spec.SchemaRef {
	if len(schema.Value.Properties) > 0 {
		return schema.Value.Properties
	}
	_, referredSchema, err := swagger.GetReferredSchema(schema)
	if err != nil {
		return nil
	}
	if referredSchema.Value != nil {
		return referredSchema.GetProperties(swagger)
	}
	if len(schema.Value.AllOf) > 0 {
		properties := make(map[string]*spec.SchemaRef)
		for _, s := range schema.Value.AllOf {
			p := (SchemaRef)(*s).GetProperties(swagger)
			for k, v := range p {
				properties[k] = v
			}
		}

		return properties
	}
	return nil
}

// Prases the object against this schema. If the obj and schema doesn't match
// return an error. Otherwise parse all the objects identified by the schema
// into the map indexed by the object class name.
func (schema SchemaRef) Parses(name string, object interface{}, collection map[string][]interface{}, followRef bool, swagger *Swagger) error {
	raiseError := func(msg string) error {
		schemaBytes, _ := json.MarshalIndent(schema.Value, "", "    ")
		objectBytes, _ := json.MarshalIndent(object, "", "    ")
		return errors.New(fmt.Sprintf(
			"schema and object don't match - %s\nSchema:\n%s\nObject:\n%s\n",
			msg, string(schemaBytes), string(objectBytes)))
	}
	if object == nil {
		return nil
	}
	refName, referredSchema, err := swagger.GetReferredSchema(schema)
	if err != nil {
		return err
	}
	if referredSchema.Value != nil {
		if !followRef {
			return nil
		}
		return referredSchema.Parses(refName, object, collection, followRef, swagger)
	}

	if len(schema.Value.AllOf) > 0 {
		// AllOf can only be combining several objects.
		objMap, objIsMap := object.(map[string]interface{})
		if !objIsMap || objMap == nil {
			// We don't consider null a valid match
			return raiseError("object is not a map")
		}
		count := 0 // keep track of how many of object's properties are accounted for.
		for _, s := range schema.Value.AllOf {
			p := ((SchemaRef)(*s)).GetProperties(swagger)
			if len(p) == 0 {
				continue
			}
			m := make(map[string]interface{})
			for k := range p {
				if v, ok := objMap[k]; ok {
					m[k] = v
					count++
				}
			}
			// The name doesn't get passed down. The name is handled at the current level.
			err = ((SchemaRef)(*s)).Parses("", m, collection, followRef, swagger)
			if err != nil {
				return err
			}
		}
		if count*4 < len(objMap)*3 {
			// This is a bit fuzzy. Sometimes it's ok for the object to have a few more fields than
			// the schema. On the other hand, the schema frequently doesn't have the "required" field.
			// So we allow a bit margin here but the object's fields can't have too many fields that
			// aren't in the schema.
			return raiseError("too many mismatched fields")
		}

		// AllOf is satisfied. We can add the whole object to our collection
		if len(name) > 0 {
			collection[name] = append(collection[name], object)
		}
		return nil
	}

	isProperty := true
	k := reflect.TypeOf(object).Kind()
	if k == reflect.Bool {
		if !strings.Contains(schema.Value.Type, gojsonschema.TYPE_BOOLEAN) {
			return raiseError("schema is not a boolean")
		}
	} else if k >= reflect.Int && k <= reflect.Uint64 {
		if !strings.Contains(schema.Value.Type, gojsonschema.TYPE_INTEGER) && !strings.Contains(schema.Value.Type, gojsonschema.TYPE_NUMBER) {
			return raiseError("schema is not an integer")
		}
		if !Validate(schema, object) {
			return raiseError("integer validation failed")
		}
	} else if k == reflect.Float32 || k == reflect.Float64 {
		// After unmarshal, the map only holds floats. It doesn't differentiate int and float.
		if !strings.Contains(schema.Value.Type, gojsonschema.TYPE_INTEGER) && !strings.Contains(schema.Value.Type, gojsonschema.TYPE_NUMBER) {
			return raiseError("schema is not a floating point number")
		}
		if !Validate(schema, object) {
			return raiseError("float validation failed")
		}
	} else if k == reflect.String {
		bothAreNumbers := reflect.TypeOf(object).String() == "json.Number" && (strings.Contains(schema.Value.Type, gojsonschema.TYPE_INTEGER) || strings.Contains(schema.Value.Type, gojsonschema.TYPE_NUMBER))
		if strings.Contains(schema.Value.Type, gojsonschema.TYPE_STRING) {
			if !Validate(schema, object) {
				return raiseError("string validation failed")
			}
		} else if !bothAreNumbers {
			return raiseError("schema is not a number")
		}
	} else if k == reflect.Map {
		isProperty = false
		objMap, objIsMap := object.(map[string]interface{})
		if !objIsMap { // || !strings.Contains(schema.Value.Type, gojsonschema.TYPE_OBJECT) {
			return raiseError("schema is not an object")
		}
		for _, requiredName := range schema.Value.Required {
			if _, exist := objMap[requiredName]; !exist {
				return raiseError(fmt.Sprintf("required field not present: %s", requiredName))
			}
		}
		// Check all the properties of the object and make sure that they can be found on the schema.
		count := 0
		for propertyName, objProperty := range objMap {
			propertySchema, exist := schema.Value.Properties[propertyName]
			if exist {
				count++
				err = ((SchemaRef)(*propertySchema)).Parses("", objProperty, collection, followRef, swagger)
				if err != nil {
					return err
				}
			}
		}
		if count*4 < len(objMap)*3 {
			return raiseError("too many mis-matched fields")
		}

		// all the properties are OK.
		if len(name) > 0 {
			collection[name] = append(collection[name], object)
		}
	} else if k == reflect.Array || k == reflect.Slice {
		isProperty = false
		if !strings.Contains(schema.Value.Type, gojsonschema.TYPE_ARRAY) {
			return raiseError("schema is not an array")
		}
		// Check the array elements.
		itemsSchema := (SchemaRef)(*schema.Value.Items)
		if itemsSchema.Value == nil {
			return raiseError("item schema is null")
		}
		ar := object.([]interface{})
		for _, item := range ar {
			err = itemsSchema.Parses("", item, collection, followRef, swagger)
			if err != nil {
				return err
			}
		}
	} else {
		return raiseError(fmt.Sprintf("unknown type: %v", k))
	}
	if isProperty && !followRef {
		tag := GetMeqaTag(schema.Value.Description)
		if tag != nil && len(tag.Class) > 0 && len(tag.Property) > 0 {
			key := fmt.Sprintf("%s.%s", tag.Class, tag.Property)
			collection[key] = append(collection[key], object)
		}
	}
	return nil
}

// Matches checks if the Schema matches the input interface. In proper swagger.json
// Enums should have types as well. So we don't check for untyped enums.
// TODO check format, handle AllOf, AnyOf, OneOf
func (schema SchemaRef) Matches(object interface{}, swagger *Swagger) bool {
	err := schema.Parses("", object, make(map[string][]interface{}), true, swagger)
	return err == nil
}

func (schema SchemaRef) Contains(name string, swagger *Swagger) bool {
	iterFunc := func(swagger *Swagger, schemaName string, schema SchemaRef, context interface{}) error {
		// The only way we have to abort is through an error.
		if schemaName == name {
			return errors.New("found")
		}
		return nil
	}

	err := schema.Iterate(iterFunc, nil, swagger, true)
	if err != nil && err.Error() == "found" {
		return true
	}
	return false
}

type SchemaIterator func(swagger *Swagger, schemaName string, schema SchemaRef, context interface{}) error

// IterateSchema descends down the starting schema and call the iterator function for all the child schemas.
// The iteration order is parent first then children. It will abort on error. The followWeak flag indicates whether
// we should follow weak references when iterating.
func (schema SchemaRef) Iterate(iterFunc SchemaIterator, context interface{}, swagger *Swagger, followWeak bool) error {
	tag := GetMeqaTag(schema.Value.Description)
	if tag != nil && (tag.Flags&FlagWeak) != 0 && !followWeak {
		return nil
	}

	err := iterFunc(swagger, "", schema, context)
	if err != nil {
		return err
	}

	if len(schema.Value.AllOf) > 0 {
		for _, s := range schema.Value.AllOf {
			err = ((SchemaRef)(*s)).Iterate(iterFunc, context, swagger, followWeak)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// Deal with refs.
	referenceName, referredSchema, err := swagger.GetReferredSchema(schema)
	if err != nil {
		return err
	}
	if referredSchema.Value != nil {
		tag := GetMeqaTag(referredSchema.Value.Description)
		if tag != nil && (tag.Flags&FlagWeak) != 0 && !followWeak {
			return nil
		}
		// We don't want to go down nested schemas.
		// XXX
		return iterFunc(swagger, referenceName, referredSchema, context)
		// return referredSchema.Iterate(iterFunc, context, swagger)
	}

	if strings.Contains(schema.Value.Type, gojsonschema.TYPE_OBJECT) {
		for _, v := range schema.Value.Properties {
			err = (SchemaRef)(*v).Iterate(iterFunc, context, swagger, followWeak)
			if err != nil {
				return err
			}
		}
	}
	if strings.Contains(schema.Value.Type, gojsonschema.TYPE_ARRAY) {
		itemSchema := (*schema.Value.Items)
		err = (SchemaRef)(itemSchema).Iterate(iterFunc, context, swagger, followWeak)
		if err != nil {
			return err
		}
	}
	return nil
}

func Validate(s SchemaRef, c interface{}) bool {
	if s.Value.Type == gojsonschema.TYPE_STRING {
		length := uint64(utf8.RuneCountInString(c.(string)))
		if s.Value.MinLength > length || (s.Value.MaxLength != nil && length > *s.Value.MaxLength) {
			return false
		}
	} else if s.Value.Type == gojsonschema.TYPE_NUMBER || s.Value.Type == gojsonschema.TYPE_INTEGER {
		if (s.Value.Min != nil && *s.Value.Min > c.(float64)) || (s.Value.Max != nil && c.(float64) > *s.Value.Max) {
			return false
		}
	}
	if len(s.Value.Pattern) > 0 {
		if ok, _ := regexp.MatchString(s.Value.Pattern, fmt.Sprint(c)); !ok {
			return false
		}
	}
	return true
}

type DBEntry struct {
	Data         map[string]interface{}            // The object itself.
	Associations map[string]map[string]interface{} // The objects associated with this object. Class to object map.
}

func (entry *DBEntry) Matches(criteria interface{}, associations map[string]map[string]interface{}, matches MatchFunc) bool {
	for className, classAssociation := range associations {
		if !mqutil.InterfaceEquals(classAssociation, entry.Associations[className]) {
			return false
		}
	}
	return matches(criteria, entry.Data)
}

// SchemaDB is our in-memory DB. It is organized around Schemas. Each schema maintains a list of objects that matches
// the schema. We don't build indexes and do linear search. This keeps the searching flexible for now.
type SchemaDB struct {
	Name      string
	Schema    SchemaRef
	NoHistory bool
	Objects   []*DBEntry
}

// Insert inserts an object into the schema's object list.
func (db *SchemaDB) Insert(obj interface{}, associations map[string]map[string]interface{}) error {
	if !db.NoHistory {
		dbentry := &DBEntry{obj.(map[string]interface{}), associations}
		db.Objects = append(db.Objects, dbentry)
	}
	return nil
}

// MatchFunc checks whether the input criteria and an input object matches.
type MatchFunc func(criteria interface{}, existing interface{}) bool

func MatchAlways(criteria interface{}, existing interface{}) bool {
	return true
}

// Clone this one but not the objects.
func (db *SchemaDB) CloneSchema() *SchemaDB {
	return &SchemaDB{db.Name, db.Schema, db.NoHistory, nil}
}

// Find finds the specified number of objects that match the input criteria.
func (db *SchemaDB) Find(criteria interface{}, associations map[string]map[string]interface{}, matches MatchFunc, desiredCount int) []interface{} {
	var result []interface{}
	for _, entry := range db.Objects {
		if entry.Matches(criteria, associations, matches) {
			result = append(result, entry.Data)
			if desiredCount >= 0 && len(result) >= desiredCount {
				return result
			}
		}
	}
	return result
}

// Delete deletes the specified number of elements that match the criteria. Input -1 for delete all.
// Returns the number of elements deleted.
func (db *SchemaDB) Delete(criteria interface{}, associations map[string]map[string]interface{}, matches MatchFunc, desiredCount int) int {
	count := 0
	for i, entry := range db.Objects {
		if entry.Matches(criteria, associations, matches) {
			db.Objects[i] = db.Objects[count]
			count++
			if desiredCount >= 0 && count >= desiredCount {
				break
			}
		}
	}
	db.Objects = db.Objects[count:]
	return count
}

// Update finds the matching object, then update with the new one.
func (db *SchemaDB) Update(criteria interface{}, associations map[string]map[string]interface{},
	matches MatchFunc, newObj map[string]interface{}, desiredCount int, patch bool) int {

	count := 0
	for _, entry := range db.Objects {
		if entry.Matches(criteria, associations, matches) {
			if patch {
				mqutil.MapCombine(entry.Data, newObj)
			} else {
				entry.Data = newObj
			}
			count++
			if desiredCount >= 0 && count >= desiredCount {
				break
			}
		}
	}
	return count
}

type DB struct {
	schemas map[string](*SchemaDB)
	Swagger *Swagger
	mutex   sync.Mutex // We don't expect much contention, as such mutex will be fast
}

// TODO it seems that if an object is not being used as a parameter to any operation, we don't
// need to track it in DB. This will save some resources. We can do this by adding swagger to
// a dag, then iterate through all the objects, and find those that doesn't have any oepration
// as a child.
func (db *DB) Init(s *Swagger) {
	db.Swagger = s
	db.schemas = make(map[string](*SchemaDB))
	for schemaName, schema := range s.Components.Schemas {
		if _, ok := db.schemas[schemaName]; ok {
			mqutil.Logger.Printf("warning - schema %s already exists", schemaName)
		}
		// Note that schema variable is reused in the loop
		schemaCopy := (SchemaRef)(*schema)
		db.schemas[schemaName] = &SchemaDB{schemaName, schemaCopy, false, nil}
	}
}

// Clone the db but not the objects
func (db *DB) CloneSchema() *DB {
	schemas := make(map[string]*SchemaDB)
	for k, v := range db.schemas {
		schemas[k] = v.CloneSchema()
	}
	return &DB{schemas, db.Swagger, sync.Mutex{}}
}

func (db *DB) GetSchema(name string) SchemaRef {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	if db.schemas[name] == nil {
		return SchemaRef{}
	}
	return db.schemas[name].Schema
}

func CopyWithoutClass(src map[string]map[string]interface{}, className string) map[string]map[string]interface{} {
	dst := make(map[string]map[string]interface{})
	for k, v := range src {
		if k != className {
			dst[k] = v
		}
	}
	return dst
}

func (db *DB) Insert(name string, obj interface{}, associations map[string]map[string]interface{}) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return mqutil.NewError(mqutil.ErrInternal, fmt.Sprintf("inserting into non-existing schema: %s", name))
	}
	return db.schemas[name].Insert(obj, CopyWithoutClass(associations, name))
}

func (db *DB) Find(name string, criteria interface{}, associations map[string]map[string]interface{},
	matches MatchFunc, desiredCount int) []interface{} {

	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return nil
	}
	return db.schemas[name].Find(criteria, CopyWithoutClass(associations, name), matches, desiredCount)
}

func (db *DB) Delete(name string, criteria interface{}, associations map[string]map[string]interface{},
	matches MatchFunc, desiredCount int) int {

	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return 0
	}
	return db.schemas[name].Delete(criteria, CopyWithoutClass(associations, name), matches, desiredCount)
}

func (db *DB) Update(name string, criteria interface{}, associations map[string]map[string]interface{},
	matches MatchFunc, newObj map[string]interface{}, desiredCount int, patch bool) int {

	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return 0
	}
	return db.schemas[name].Update(criteria, CopyWithoutClass(associations, name), matches, newObj, desiredCount, patch)
}

// FindMatchingSchema finds the schema that matches the obj.
func (db *DB) FindMatchingSchema(obj interface{}) (string, SchemaRef) {
	for name, schemaDB := range db.schemas {
		schema := schemaDB.Schema
		if schema.Matches(obj, db.Swagger) {
			mqutil.Logger.Printf("found matching schema: %s", name)
			return name, (SchemaRef)(schema)
		}
	}
	return "", SchemaRef{}
}

// DB holds schema name to Schema mapping.
var ObjDB DB

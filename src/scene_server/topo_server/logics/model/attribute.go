/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"configcenter/src/ac/extensions"
	"configcenter/src/apimachinery"
	"configcenter/src/common"
	"configcenter/src/common/auditlog"
	"configcenter/src/common/blog"
	ccErr "configcenter/src/common/errors"
	"configcenter/src/common/http/rest"
	"configcenter/src/common/language"
	"configcenter/src/common/mapstr"
	"configcenter/src/common/mapstruct"
	"configcenter/src/common/metadata"
	"configcenter/src/common/util"
)

// AttributeOperationInterface attribute operation methods
type AttributeOperationInterface interface {
	CreateObjectAttribute(kit *rest.Kit, data *metadata.Attribute) (*metadata.Attribute, error)
	CreateTableObjectAttribute(kit *rest.Kit, data *metadata.Attribute) (*metadata.Attribute, error)
	DeleteObjectAttribute(kit *rest.Kit, cond mapstr.MapStr, modelBizID int64) error
	UpdateObjectAttribute(kit *rest.Kit, data mapstr.MapStr, attID int64, modelBizID int64) error
	// CreateObjectBatch upsert object attributes
	CreateObjectBatch(kit *rest.Kit, data map[string]metadata.ImportObjectData) (mapstr.MapStr, error)
	// FindObjectBatch find object to attributes mapping
	FindObjectBatch(kit *rest.Kit, objIDs []string) (mapstr.MapStr, error)
	ValidObjIDAndInstID(kit *rest.Kit, objID string, option interface{}, isMultiple bool) error
	SetProxy(grp GroupOperationInterface, obj ObjectOperationInterface)
}

// NewAttributeOperation create a new attribute operation instance
func NewAttributeOperation(client apimachinery.ClientSetInterface, authManager *extensions.AuthManager,
	languageIf language.CCLanguageIf) AttributeOperationInterface {
	return &attribute{
		clientSet:   client,
		authManager: authManager,
		lang:        languageIf,
	}
}

type attribute struct {
	lang        language.CCLanguageIf
	clientSet   apimachinery.ClientSetInterface
	authManager *extensions.AuthManager
	grp         GroupOperationInterface
	obj         ObjectOperationInterface
}

// SetProxy SetProxy
func (a *attribute) SetProxy(grp GroupOperationInterface, obj ObjectOperationInterface) {
	a.grp = grp
	a.obj = obj
}

// getEnumQuoteOption get enum quote field option bk_obj_id and bk_inst_id value
func (a *attribute) getEnumQuoteOption(kit *rest.Kit, option interface{}, isMultiple bool) (string, []int64, error) {
	if option == nil {
		return "", nil, kit.CCError.Errorf(common.CCErrCommParamsLostField, "option")
	}

	arrOption, ok := option.([]interface{})
	if !ok {
		blog.Errorf("option %v not enum quote option, rid: %s", option, kit.Rid)
		return "", nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, "option")
	}
	if len(arrOption) == 0 {
		return "", nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, "option")
	}

	if !isMultiple && len(arrOption) != 1 {
		blog.Errorf("enum option is single choice, but arr option value is multiple, rid: %s", kit.Rid)
		return "", nil, kit.CCError.CCError(common.CCErrCommParamsNeedSingleChoice)
	}

	if len(arrOption) > common.AttributeOptionArrayMaxLength {
		blog.Errorf("option array length %d exceeds max length %d, rid: %s", len(arrOption),
			common.AttributeOptionArrayMaxLength, kit.Rid)
		return "", nil, kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed, "option",
			common.AttributeOptionArrayMaxLength)
	}

	var quoteObjID string
	instIDMap := make(map[int64]interface{}, 0)
	for _, o := range arrOption {
		mapOption, ok := o.(map[string]interface{})
		if !ok || mapOption == nil {
			blog.Errorf("enum quote option %v must contain bk_obj_id and bk_inst_id, rid: %s", option, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, "option")
		}
		objIDVal, objIDOk := mapOption[common.BKObjIDField]
		if !objIDOk || objIDVal == "" {
			blog.Errorf("enum quote option bk_obj_id can't be empty, rid: %s", option, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsNeedSet, "option bk_obj_id")
		}
		objID, ok := objIDVal.(string)
		if !ok {
			blog.Errorf("objIDVal %v not string, rid: %s", objIDVal, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsNeedString, "option bk_obj_id")
		}
		if common.AttributeOptionValueMaxLength < utf8.RuneCountInString(objID) {
			blog.Errorf("option bk_obj_id %s length %d exceeds max length %d, rid: %s", objID,
				utf8.RuneCountInString(objID), common.AttributeOptionValueMaxLength, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed, "option bk_obj_id",
				common.AttributeOptionValueMaxLength)
		}

		if quoteObjID == "" {
			quoteObjID = objID
		} else if quoteObjID != objID {
			blog.Errorf("enum quote objID not unique, objID: %s, rid: %s", objID, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, "quote objID")
		}

		instIDVal, instIDOk := mapOption[common.BKInstIDField]
		if !instIDOk || instIDVal == "" {
			blog.Errorf("enum quote option bk_inst_id can't be empty, rid: %s", option, kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsNeedSet, "option bk_inst_id")
		}

		switch mapOption["type"] {
		case "int":
			instID, err := util.GetInt64ByInterface(instIDVal)
			if err != nil {
				return "", nil, err
			}
			if instID == 0 {
				return "", nil, fmt.Errorf("enum quote instID is %d, it is illegal", instID)
			}
			instIDMap[instID] = struct{}{}
		default:
			blog.Errorf("enum quote option type must be 'int', current: %v, rid: %s", mapOption["type"], kit.Rid)
			return "", nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, "option type")
		}
	}

	instIDs := make([]int64, 0)
	for instID := range instIDMap {
		instIDs = append(instIDs, instID)
	}

	return quoteObjID, instIDs, nil
}

// enumQuoteCanNotUseModel 校验引用模型不能为集群、模块、进程、容器、自定义层级相关模型
func (a *attribute) enumQuoteCanNotUseModel(kit *rest.Kit, objID string) error {

	// 校验引用模型不能为集群，模块，进程内置模型 TODO 容器相关的模型暂无定义，后续添加
	switch objID {
	case common.BKInnerObjIDSet, common.BKInnerObjIDModule, common.BKInnerObjIDProc:
		return fmt.Errorf("enum quote obj can not inner model")
	}

	// 校验引用模型不能为自定义层级模型
	query := &metadata.QueryCondition{
		Fields: []string{common.BKObjIDField, common.BKAsstObjIDField},
		Condition: mapstr.MapStr{
			common.AssociationKindIDField: common.AssociationKindMainline,
		},
		DisableCounter: true,
	}
	mainlineAsstRsp, err := a.clientSet.CoreService().Association().ReadModelAssociation(kit.Ctx, kit.Header, query)
	if err != nil {
		blog.Errorf("search mainline association failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}

	mainlineObjectChildMap := make(map[string]string, 0)
	for _, asst := range mainlineAsstRsp.Info {
		if asst.ObjectID == common.BKInnerObjIDHost {
			continue
		}
		mainlineObjectChildMap[asst.AsstObjID] = asst.ObjectID
	}

	objectIDs := make([]string, 0)
	for objectID := common.BKInnerObjIDApp; len(objectID) != 0; objectID = mainlineObjectChildMap[objectID] {
		if objectID == common.BKInnerObjIDApp || objectID == common.BKInnerObjIDSet ||
			objectID == common.BKInnerObjIDModule {
			continue
		}
		objectIDs = append(objectIDs, objectID)
	}

	for _, customObjID := range objectIDs {
		if objID == customObjID {
			return fmt.Errorf("enum quote obj can not custom model")
		}
	}
	return nil
}

// ValidObjIDAndInstID check obj is inner model and obj is exist, inst is exit
func (a *attribute) ValidObjIDAndInstID(kit *rest.Kit, objID string, option interface{}, isMultiple bool) error {
	quoteObjID, instIDs, err := a.getEnumQuoteOption(kit, option, isMultiple)
	if err != nil {
		blog.Errorf("get enum quote option value failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}
	if quoteObjID == "" || len(instIDs) == 0 {
		return fmt.Errorf("enum quote objID or instID is empty, objIDs: %s, instIDs: %v", quoteObjID, instIDs)
	}

	isObjExists, err := a.obj.IsObjectExist(kit, quoteObjID)
	if err != nil {
		blog.Errorf("check obj id is exist failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}
	if !isObjExists {
		blog.Errorf("enum quote option bk_obj_id is not exist, objID: %s, rid: %s", quoteObjID, kit.Rid)
		return fmt.Errorf("enum quote objID is not exist, objID: %s", quoteObjID)
	}

	if quoteObjID == objID {
		blog.Errorf("enum quote model can not model self, objID: %s, rid: %s", objID, kit.Rid)
		return fmt.Errorf("enum quote model can not model self, objID: %s", objID)
	}

	// 集群，模块，进程，容器，自定义层级模块不能被引用
	if err := a.enumQuoteCanNotUseModel(kit, quoteObjID); err != nil {
		blog.Errorf("enum quote model can not use some inner model, err: %v, rid: %s", err, kit.Rid)
		return err
	}

	input := &metadata.QueryCondition{
		Fields: []string{common.GetInstIDField(quoteObjID)},
		Condition: mapstr.MapStr{
			common.GetInstIDField(quoteObjID): mapstr.MapStr{common.BKDBIN: instIDs},
		},
	}
	resp, err := a.clientSet.CoreService().Instance().ReadInstance(kit.Ctx, kit.Header, quoteObjID, input)
	if err != nil {
		blog.Errorf("get inst data failed, input: %+v, err: %v, rid: %s", input, err, kit.Rid)
		return err
	}
	if resp.Count == 0 {
		blog.Errorf("enum quote option inst not exist, input: %+v, rid: %s", input, kit.Rid)
		return fmt.Errorf("enum quote inst not exist, instIDs: %v", instIDs)
	}

	return nil
}

func (a *attribute) validTableAttributes(kit *rest.Kit, option interface{}) error {

	if option == nil {
		return errors.New("option params is invalid")
	}

	tableOption, err := parseTableAttrOption(option)
	if err != nil {
		blog.Errorf("get attribute option failed, error: %v, option: %v, rid: %s", err, kit.Rid)
		return err
	}

	headerAttrMap, err := a.getTableAttrHeaderDetail(kit, tableOption.Header)
	if err != nil {
		return err
	}

	if err := a.ValidTableAttrDefaultValue(kit, tableOption.Default, headerAttrMap); err != nil {
		return err
	}
	return nil
}

// getTableAttrHeaderDetail in the creation and update scenarios,
// the full amount of header content needs to be passed.
func (a *attribute) getTableAttrHeaderDetail(kit *rest.Kit, header []metadata.Attribute) (
	map[string]*metadata.Attribute, error) {

	if len(header) == 0 {
		return nil, errors.New("table header must be set")
	}

	if len(header) > metadata.TableHeaderMaxNum {
		return nil, fmt.Errorf("the header field length of the table cannot exceed %d", metadata.TableHeaderMaxNum)
	}

	propertyAttr := make(map[string]*metadata.Attribute)
	var longCharNum int
	for index := range header {
		// determine whether the underlying type is legal
		if !metadata.ValidTableFieldBaseType(header[index].PropertyType) {
			return nil, fmt.Errorf("table header type is invalid, type : %v", header[index].PropertyType)
		}
		// the number of long characters in the basic type of the table
		// field type cannot exceed the maximum value supported by the system.
		if header[index].PropertyType == common.FieldTypeLongChar {
			longCharNum++
		}
		if longCharNum > metadata.TableLongCharMaxNum {
			return nil, fmt.Errorf("exceeds the maximum number(%d) of long characters supported by the table"+
				" field header", metadata.TableLongCharMaxNum)
		}
		// check if property type for creation is valid, can't update property type
		if header[index].PropertyType == "" {
			return nil, kit.CCError.Errorf(common.CCErrCommParamsNeedSet, metadata.AttributeFieldPropertyType)
		}

		if header[index].PropertyID == "" {
			return nil, kit.CCError.Errorf(common.CCErrCommParamsNeedSet, metadata.AttributeFieldPropertyID)
		}

		if common.AttributeIDMaxLength < utf8.RuneCountInString(header[index].PropertyID) {
			return nil, kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed,
				a.lang.CreateDefaultCCLanguageIf(util.GetLanguage(kit.Header)).Language(
					"model_attr_bk_property_id"), common.AttributeIDMaxLength)
		}

		match, err := regexp.MatchString(common.FieldTypeStrictCharRegexp, header[index].PropertyID)
		if err != nil {
			return nil, err
		}

		if !match {
			return nil, kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, header[index].PropertyID)
		}
		if header[index].PropertyName == "" {
			return nil, kit.CCError.Errorf(common.CCErrCommParamsNeedSet, metadata.AttributeFieldPropertyName)
		}
		if common.AttributeNameMaxLength < utf8.RuneCountInString(header[index].PropertyName) {
			return nil, kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed,
				a.lang.CreateDefaultCCLanguageIf(util.GetLanguage(kit.Header)).Language(
					"model_attr_bk_property_name"), common.AttributeNameMaxLength)
		}
		err = validTableTypeOption(header[index].PropertyType, header[index].Option, header[index].Default,
			header[index].IsMultiple, kit.CCError)
		if err != nil {
			return nil, err
		}
		propertyAttr[header[index].PropertyID] = &header[index]
	}
	return propertyAttr, nil
}

// ToDo：临时判断，合入最新代码之后需要调整
func validTableTypeOption(propertyType string, option, defaultValue interface{}, isMultiple *bool,
	errProxy ccErr.DefaultCCErrorIf) error {
	bFalse := false
	if isMultiple == nil {
		isMultiple = &bFalse
	}

	switch propertyType {
	case common.FieldTypeInt:
		return util.ValidFieldTypeInt(option, defaultValue, "", errProxy)
	case common.FieldTypeEnumMulti:
		return util.ValidFieldTypeEnumOption(option, *isMultiple, "", errProxy)
	case common.FieldTypeLongChar, common.FieldTypeSingleChar:
		return util.ValidFieldTypeString(option, defaultValue, "", errProxy)
	case common.FieldTypeFloat:
		return util.ValidFieldTypeFloat(option, defaultValue, "", errProxy)
	case common.FieldTypeBool:
		// todo
	default:
		return fmt.Errorf("property type(%#v) is error", propertyType)
	}
	return nil
}

// ValidTableAttrDefaultValue attr: key is property_id, value is the corresponding header content.
func (a *attribute) ValidTableAttrDefaultValue(kit *rest.Kit, defaultValue []map[string]interface{},
	attr map[string]*metadata.Attribute) error {

	if len(defaultValue) == 0 {
		return nil
	}
	if len(defaultValue) > metadata.TableDefaultMaxLines {
		return fmt.Errorf("the number of rows of the default value in the table attribute exceeds the maximum "+
			"value(%v) supported by the system", metadata.TableDefaultMaxLines)
	}
	// judge the legality of each field of the default
	// value according to the attributes of the header.
	for _, value := range defaultValue {
		for k, v := range value {
			if err := attr[k].ValidTableDefaultAttr(kit.Ctx, v); err.ErrCode != 0 {
				return err.ToCCError(kit.CCError)
			}
		}
	}
	return nil
}

func parseTableAttrOption(option interface{}) (*metadata.TableAttributesOption, error) {
	marshaledOptions, err := json.Marshal(option)
	if err != nil {
		return nil, err
	}

	result := new(metadata.TableAttributesOption)
	if err := json.Unmarshal(marshaledOptions, result); err != nil {
		return nil, err
	}
	return result, nil
}

// isValid check is valid
func (a *attribute) isValid(kit *rest.Kit, isUpdate bool, data *metadata.Attribute) error {
	if data.PropertyID == common.BKInstParentStr {
		return nil
	}

	if (isUpdate && data.IsMultiple != nil) || !isUpdate {
		if err := util.ValidPropertyTypeIsMultiple(data.PropertyType, *data.IsMultiple, kit.CCError); err != nil {
			return err
		}
	}

	// 用户类型字段，在创建的时候默认是支持可多选的，而且这个字段是否可多选在页面是不可配置的,所以在创建的时候将值置为true
	if data.PropertyType == common.FieldTypeUser && !isUpdate {
		isMultiple := true
		data.IsMultiple = &isMultiple
	}

	// check if property type for creation is valid, can't update property type
	if !isUpdate && data.PropertyType == "" {
		return kit.CCError.Errorf(common.CCErrCommParamsNeedSet, metadata.AttributeFieldPropertyType)
	}

	if !isUpdate || data.PropertyID != "" {
		if common.AttributeIDMaxLength < utf8.RuneCountInString(data.PropertyID) {
			return kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed,
				a.lang.CreateDefaultCCLanguageIf(util.GetLanguage(kit.Header)).Language(
					"model_attr_bk_property_id"), common.AttributeIDMaxLength)
		}
		match, err := regexp.MatchString(common.FieldTypeStrictCharRegexp, data.PropertyID)
		if err != nil {
			return err
		}

		if !match {
			return kit.CCError.Errorf(common.CCErrCommParamsIsInvalid, data.PropertyID)
		}
	}

	if !isUpdate || data.PropertyName != "" {
		if common.AttributeNameMaxLength < utf8.RuneCountInString(data.PropertyName) {
			return kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed,
				a.lang.CreateDefaultCCLanguageIf(util.GetLanguage(kit.Header)).Language(
					"model_attr_bk_property_name"), common.AttributeNameMaxLength)
		}
	}

	// check option validity for creation,
	// update validation is in coreservice cause property type need to be obtained from db
	if !isUpdate && a.isPropertyTypeIntEnumListSingleLong(data.PropertyType) {
		if err := util.ValidPropertyOption(data.PropertyType, data.Option, *data.IsMultiple,
			data.Default, kit.Rid, kit.CCError); err != nil {
			return err
		}
	}

	// check enum quote field option validity creation or update
	if data.PropertyType == common.FieldTypeEnumQuote && data.IsMultiple != nil {
		if err := a.ValidObjIDAndInstID(kit, data.ObjectID, data.Option, *data.IsMultiple); err != nil {
			blog.Errorf("check objID and instID failed, err: %v, rid: %s", err, kit.Rid)
			return err
		}
	}

	if data.Placeholder != "" && common.AttributePlaceHolderMaxLength < utf8.RuneCountInString(data.Placeholder) {
		return kit.CCError.Errorf(common.CCErrCommValExceedMaxFailed,
			a.lang.CreateDefaultCCLanguageIf(util.GetLanguage(kit.Header)).Language("model_attr_placeholder"),
			common.AttributePlaceHolderMaxLength)
	}

	return nil
}

// isPropertyTypeIntEnumListSingleLong check is property type in enum list single long
func (a *attribute) isPropertyTypeIntEnumListSingleLong(propertyType string) bool {
	switch propertyType {
	case common.FieldTypeInt, common.FieldTypeEnum, common.FieldTypeList, common.FieldTypeEnumMulti:
		return true
	case common.FieldTypeSingleChar, common.FieldTypeLongChar:
		return true
	default:
		return false
	}
}

// checkAttributeGroupExist check attribute group exist, not exist create default group
func (a *attribute) checkAttributeGroupExist(kit *rest.Kit, data *metadata.Attribute) error {
	cond := []map[string]interface{}{{
		common.BKObjIDField:           data.ObjectID,
		common.BKPropertyGroupIDField: data.PropertyGroup,
		common.BKAppIDField:           data.BizID,
	}}

	defCntRes, err := a.clientSet.CoreService().Count().GetCountByFilter(kit.Ctx, kit.Header,
		common.BKTableNamePropertyGroup, cond)
	if err != nil {
		blog.Errorf("get attribute group count by cond(%#v) failed, err: %v, rid: %s", cond, err, kit.Rid)
		return err
	}

	if len(defCntRes) != 1 {
		blog.Errorf("get attr group count by cond(%#v) returns not one result, err: %v, rid: %s", cond, err, kit.Rid)
		return kit.CCError.Error(common.CCErrCommNotFound)
	}

	if defCntRes[0] > 0 {
		return nil
	}

	if data.BizID == 0 {
		data.PropertyGroup = common.BKDefaultField
		return nil
	}

	// create the biz default group if it is not exist
	bizDefaultGroupCond := []map[string]interface{}{{
		common.BKObjIDField:           data.ObjectID,
		common.BKPropertyGroupIDField: common.BKBizDefault,
		common.BKAppIDField:           data.BizID,
	}}

	bizDefCntRes, err := a.clientSet.CoreService().Count().GetCountByFilter(kit.Ctx, kit.Header,
		common.BKTableNamePropertyGroup, bizDefaultGroupCond)
	if err != nil {
		blog.Errorf("get attribute group count by cond(%#v) failed, err: %v, rid: %s", cond, err, kit.Rid)
		return err
	}

	if len(bizDefCntRes) != 1 {
		blog.Errorf("get attr group count by cond(%#v) returns not one result, err: %v, rid: %s", cond, err, kit.Rid)
		return kit.CCError.Error(common.CCErrCommNotFound)
	}

	if bizDefCntRes[0] == 0 {
		group := metadata.Group{
			IsDefault:  true,
			GroupIndex: -1,
			GroupName:  common.BKBizDefault,
			GroupID:    common.BKBizDefault,
			ObjectID:   data.ObjectID,
			OwnerID:    data.OwnerID,
			BizID:      data.BizID,
		}

		if _, err := a.grp.CreateObjectGroup(kit, &group); err != nil {
			blog.Errorf("failed to create the default group, err: %s, rid: %s", err, kit.Rid)
			return err
		}
	}

	data.PropertyGroup = common.BKBizDefault
	return nil
}

func (a *attribute) validCreateTableAttribute(kit *rest.Kit, data *metadata.Attribute) error {
	// check if the object is mainline object, if yes. then user can not create required attribute.
	yes, err := a.isMainlineModel(kit, data.ObjectID)
	if err != nil {
		blog.Errorf("not allow to add required attribute to mainline object: %+v. "+"rid: %d.", data, kit.Rid)
		return err
	}

	if yes && data.IsRequired {
		return kit.CCError.Error(common.CCErrTopoCanNotAddRequiredAttributeForMainlineModel)
	}

	// check the object id
	exist, err := a.obj.IsObjectExist(kit, data.ObjectID)
	if err != nil {
		return err
	}
	if !exist {
		blog.Errorf("obj id is not exist, obj id: %s, rid: %s", data.ObjectID, kit.Rid)
		return kit.CCError.CCErrorf(common.CCErrCommParamsIsInvalid, common.BKObjIDField)
	}

	if err = a.checkAttributeGroupExist(kit, data); err != nil {
		blog.Errorf("failed to create the default group, err: %s, rid: %s", err, kit.Rid)
		return err
	}

	if err := a.validTableAttributes(kit, data.Option); err != nil {
		return err
	}
	return nil
}

func (a *attribute) createTableModelAndAttributeGroup(kit *rest.Kit, data *metadata.Attribute) error {

	t := metadata.Now()
	obj := metadata.Object{
		ObjCls:     metadata.ClassificationTableID,
		ObjIcon:    "icon-cc-table",
		ObjectID:   data.ObjectID,
		ObjectName: data.PropertyName,
		IsHidden:   true,
		Creator:    string(metadata.FromCCSystem),
		Modifier:   string(metadata.FromCCSystem),
		CreateTime: &t,
		LastTime:   &t,
		OwnerID:    kit.SupplierAccount,
	}

	objRsp, err := a.clientSet.CoreService().Model().CreateTableModel(kit.Ctx, kit.Header,
		&metadata.CreateModel{Spec: obj, Attributes: []metadata.Attribute{*data}})
	if err != nil {
		blog.Errorf("create table object(%s) failed, err: %v, rid: %s", obj.ObjectID, err, kit.Rid)
		return err
	}

	obj.ID = int64(objRsp.Created.ID)
	objID := metadata.GenerateModelQuoteObjName(data.ObjectID, data.PropertyID)
	// create the default group
	groupData := metadata.Group{
		IsDefault:  true,
		GroupIndex: -1,
		GroupName:  "Default",
		GroupID:    NewGroupID(true),
		ObjectID:   objID,
		OwnerID:    obj.OwnerID,
	}

	_, err = a.clientSet.CoreService().Model().CreateAttributeGroup(kit.Ctx, kit.Header,
		objID, metadata.CreateModelAttributeGroup{Data: groupData})
	if err != nil {
		blog.Errorf("create attribute group[%s] failed, err: %v, rid: %s", groupData.GroupID, err, kit.Rid)
		return err
	}

	// generate audit log of object attribute group.
	audit := auditlog.NewObjectAuditLog(a.clientSet.CoreService())
	generateAuditParameter := auditlog.NewGenerateAuditCommonParameter(kit, metadata.AuditCreate)
	auditLog, err := audit.GenerateAuditLog(generateAuditParameter, obj.ID, nil)
	if err != nil {
		blog.Errorf("generate audit log failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}

	// save audit log.
	if err := audit.SaveAuditLog(kit, *auditLog); err != nil {
		blog.Errorf("save audit log failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}
	return nil
}

// CreateTableObjectAttribute create internal form fields in a separate process
func (a *attribute) CreateTableObjectAttribute(kit *rest.Kit, data *metadata.Attribute) (
	*metadata.Attribute, error) {
	if data.IsOnly {
		data.IsRequired = true
	}

	if len(data.PropertyGroup) == 0 {
		data.PropertyGroup = "default"
	}

	if err := a.validCreateTableAttribute(kit, data); err != nil {
		return nil, err
	}

	if err := a.createTableModelAndAttributeGroup(kit, data); err != nil {
		return nil, err
	}

	data.OwnerID = kit.SupplierAccount
	if err := a.createModelQuoteRelation(kit, data.ObjectID, data.PropertyID); err != nil {
		return nil, err
	}

	return data, nil
}

func (a *attribute) createModelQuoteRelation(kit *rest.Kit, objectID, propertyID string) error {
	relation := metadata.ModelQuoteRelation{
		DestModel:  metadata.GenerateModelQuoteObjID(objectID, propertyID),
		SrcModel:   objectID,
		PropertyID: propertyID,
		Type:       common.ModelQuoteType(common.FieldTypeInnerTable),
	}
	if cErr := a.clientSet.CoreService().ModelQuote().CreateModelQuoteRelation(kit.Ctx, kit.Header,
		[]metadata.ModelQuoteRelation{relation}); cErr != nil {
		blog.Errorf("created quote relation failed, relation: %#v, err: %v, rid: %s", relation, cErr, kit.Rid)
		return kit.CCError.CCError(common.CCErrTopoObjectAttributeCreateFailed)
	}
	return nil
}

// CreateObjectAttribute create object attribute
func (a *attribute) CreateObjectAttribute(kit *rest.Kit, data *metadata.Attribute) (*metadata.Attribute, error) {
	if data.IsOnly {
		data.IsRequired = true
	}

	if len(data.PropertyGroup) == 0 {
		data.PropertyGroup = "default"
	}

	// check if the object is mainline object, if yes. then user can not create required attribute.
	yes, err := a.isMainlineModel(kit, data.ObjectID)
	if err != nil {
		blog.Errorf("not allow to add required attribute to mainline object: %+v. "+"rid: %d.", data, kit.Rid)
		return nil, err
	}

	if yes && data.IsRequired {
		return nil, kit.CCError.Error(common.CCErrTopoCanNotAddRequiredAttributeForMainlineModel)
	}

	// check the object id
	exist, err := a.obj.IsObjectExist(kit, data.ObjectID)
	if err != nil {
		return nil, err
	}
	if !exist {
		blog.Errorf("obj id is not exist, obj id: %s, rid: %s", data.ObjectID, kit.Rid)
		return nil, kit.CCError.CCErrorf(common.CCErrCommParamsIsInvalid, common.BKObjIDField)
	}

	if err = a.checkAttributeGroupExist(kit, data); err != nil {
		blog.Errorf("failed to create the default group, err: %s, rid: %s", err, kit.Rid)
		return nil, err
	}

	if err := a.isValid(kit, false, data); err != nil {
		return nil, err
	}

	data.OwnerID = kit.SupplierAccount
	input := metadata.CreateModelAttributes{Attributes: []metadata.Attribute{*data}}
	resp, err := a.clientSet.CoreService().Model().CreateModelAttrs(kit.Ctx, kit.Header, data.ObjectID, &input)
	if err != nil {
		blog.Errorf("failed to create model attrs, err: %v, input: %#v, rid: %s", err, input, kit.Rid)
		return nil, kit.CCError.CCError(common.CCErrCommHTTPDoRequestFailed)
	}
	for _, exception := range resp.Exceptions {
		return nil, kit.CCError.New(int(exception.Code), exception.Message)
	}

	if len(resp.Repeated) > 0 {
		blog.Errorf("create model(%s) attr but it is duplicated, input: %#v, rid: %s", data.ObjectID, input, kit.Rid)
		return nil, kit.CCError.CCError(common.CCErrorAttributeNameDuplicated)
	}

	if len(resp.Created) != 1 {
		blog.Errorf("created model(%s) attr amount is not one, input: %#v, rid: %s", data.ObjectID, input, kit.Rid)
		return nil, kit.CCError.CCError(common.CCErrTopoObjectAttributeCreateFailed)
	}

	// get current model attribute data by id.
	attrReq := &metadata.QueryCondition{Condition: mapstr.MapStr{metadata.AttributeFieldID: int64(resp.Created[0].ID)}}
	attrRes, err := a.clientSet.CoreService().Model().ReadModelAttr(kit.Ctx, kit.Header, data.ObjectID, attrReq)
	if err != nil {
		blog.Errorf("get created model attribute by id(%d) failed, err: %v, rid: %s", resp.Created[0].ID, err, kit.Rid)
		return nil, err
	}

	if len(attrRes.Info) != 1 {
		blog.Errorf("get created model attribute by id(%d) returns not one attr, rid: %s", resp.Created[0].ID, kit.Rid)
		return nil, kit.CCError.CCError(common.CCErrCommNotFound)
	}

	data = &attrRes.Info[0]

	// generate audit log of model attribute.
	audit := auditlog.NewObjectAttributeAuditLog(a.clientSet.CoreService())
	generateAuditParameter := auditlog.NewGenerateAuditCommonParameter(kit, metadata.AuditCreate)

	auditLog, err := audit.GenerateAuditLog(generateAuditParameter, data.ID, data)
	if err != nil {
		blog.Errorf("gen audit log after creating attr %s failed, err: %v, rid: %s", data.PropertyName, err, kit.Rid)
		return nil, err
	}

	// save audit log.
	if err := audit.SaveAuditLog(kit, *auditLog); err != nil {
		blog.Errorf("save audit log after creating attr %s failed, err: %v, rid: %s", data.PropertyName, err, kit.Rid)
		return nil, err
	}

	return data, nil
}

// DeleteObjectAttribute delete object attribute
func (a *attribute) DeleteObjectAttribute(kit *rest.Kit, cond mapstr.MapStr, modelBizID int64) error {
	util.AddModelBizIDCondition(cond, modelBizID)
	queryCond := &metadata.QueryCondition{
		Condition: cond,
		Page: metadata.BasePage{
			Limit: common.BKNoLimit,
		},
	}
	attrItems, err := a.clientSet.CoreService().Model().ReadModelAttrByCondition(kit.Ctx, kit.Header, queryCond)
	if err != nil {
		blog.Errorf("failed to find the attributes by the cond(%v), err: %v, rid: %s", cond, err, kit.Rid)
		return err
	}

	if len(attrItems.Info) == 0 {
		blog.Errorf("not find the attributes by the cond(%v), rid: %s", cond, kit.Rid)
		return nil
	}

	auditLogArr := make([]metadata.AuditLog, 0)
	attrIDMap := make(map[string][]int64, 0)
	audit := auditlog.NewObjectAttributeAuditLog(a.clientSet.CoreService())
	generateAuditParameter := auditlog.NewGenerateAuditCommonParameter(kit, metadata.AuditDelete)
	for _, attrItem := range attrItems.Info {
		// generate audit log of model attribute.
		auditLog, err := audit.GenerateAuditLog(generateAuditParameter, attrItem.ID, &attrItem)
		if err != nil {
			blog.Errorf("generate audit log failed, model attribute %s, err: %v, rid: %s", attrItem.PropertyName,
				err, kit.Rid)
			return err
		}
		auditLogArr = append(auditLogArr, *auditLog)
		attrIDMap[attrItem.ObjectID] = append(attrIDMap[attrItem.ObjectID], attrItem.ID)
	}

	for objID, instIDs := range attrIDMap {
		// delete the attribute.
		deleteCond := &metadata.DeleteOption{
			Condition: mapstr.MapStr{
				common.BKFieldID: mapstr.MapStr{common.BKDBIN: instIDs},
			},
		}
		rsp, err := a.clientSet.CoreService().Model().DeleteModelAttr(kit.Ctx, kit.Header, objID, deleteCond)
		if err != nil {
			blog.Errorf("delete object attribute failed, err: %v, rid: %s", err, kit.Rid)
			return err
		}

		if ccErr := rsp.CCError(); ccErr != nil {
			blog.Errorf("delete object attribute failed, err: %v, rid: %s", ccErr, kit.Rid)
			return ccErr
		}
	}

	// save audit log.
	if err := audit.SaveAuditLog(kit, auditLogArr...); err != nil {
		blog.Errorf("delete object attribute success, but save audit log failed, err: %v, rid: %s", err, kit.Rid)
		return err
	}

	return nil
}

// UpdateObjectAttribute update object attribute
func (a *attribute) UpdateObjectAttribute(kit *rest.Kit, data mapstr.MapStr, attID int64, modelBizID int64) error {

	attr := new(metadata.Attribute)
	if err := mapstruct.Decode2Struct(data, attr); err != nil {
		blog.Errorf("unmarshal mapstr data into module failed, module: %s, err: %s, rid: %s", attr, err, kit.Rid)
		return kit.CCError.CCError(common.CCErrCommParseDBFailed)
	}

	if err := a.isValid(kit, true, attr); err != nil {
		return err
	}

	// generate audit log of model attribute.
	audit := auditlog.NewObjectAttributeAuditLog(a.clientSet.CoreService())
	generateAuditParameter := auditlog.NewGenerateAuditCommonParameter(kit, metadata.AuditUpdate).WithUpdateFields(data)
	auditLog, err := audit.GenerateAuditLog(generateAuditParameter, attID, nil)
	if err != nil {
		blog.Errorf("generate audit log failed before update model attribute, attID: %d, err: %v, rid: %s",
			attID, err, kit.Rid)
		return err
	}

	// to update.
	cond := mapstr.MapStr{
		common.BKFieldID: attID,
	}
	util.AddModelBizIDCondition(cond, modelBizID)
	input := metadata.UpdateOption{
		Condition: cond,
		Data:      data,
	}
	if _, err := a.clientSet.CoreService().Model().UpdateModelAttrsByCondition(kit.Ctx, kit.Header, &input); err != nil {
		blog.Errorf("failed to update module attr, err: %s, rid: %s", err, kit.Rid)
		return err
	}

	// save audit log.
	if err := audit.SaveAuditLog(kit, *auditLog); err != nil {
		blog.Errorf("update object attribute success, but save audit log failed, attID: %d, err: %v, rid: %s",
			attID, err, kit.Rid)
		return err
	}

	return nil
}

// isMainlineModel check is mainline model by module id
func (a *attribute) isMainlineModel(kit *rest.Kit, modelID string) (bool, error) {
	cond := mapstr.MapStr{
		common.AssociationKindIDField: common.AssociationKindMainline,
	}
	queryCond := &metadata.QueryCondition{
		Condition:      cond,
		DisableCounter: true,
	}
	asst, err := a.clientSet.CoreService().Association().ReadModelAssociation(kit.Ctx, kit.Header, queryCond)
	if err != nil {
		return false, err
	}

	if len(asst.Info) <= 0 {
		return false, fmt.Errorf("model association [%+v] not found", cond)
	}

	for _, mainline := range asst.Info {
		if mainline.ObjectID == modelID {
			return true, nil
		}
	}

	return false, nil
}

type rowInfo struct {
	Row  int64  `json:"row"`
	Info string `json:"info"`
	// value can empty, eg: parse error
	PropID string `json:"bk_property_id"`
}

type createObjectBatchObjResult struct {
	// 异常错误，比如接卸该行数据失败， 查询数据失败
	Errors []rowInfo `json:"errors,omitempty"`
	// 新加属性失败
	InsertFailed []rowInfo `json:"insert_failed,omitempty"`
	// 更新属性失败
	UpdateFailed []rowInfo `json:"update_failed,omitempty"`
	// 成功数据的信息
	Success []rowInfo `json:"success,omitempty"`
	// 失败信息，如模型不存在
	Error string `json:"error,omitempty"`
}

// CreateObjectBatch this method doesn't act as it's name, it create or update model's attributes indeed.
// it only operate on model already exist, that is to say no new model will be created.
func (a *attribute) CreateObjectBatch(kit *rest.Kit, inputDataMap map[string]metadata.ImportObjectData) (mapstr.MapStr,
	error) {

	result := mapstr.New()
	hasError := false
	for objID, inputData := range inputDataMap {
		// check if the object exists
		isObjExists, err := a.obj.IsObjectExist(kit, objID)
		if err != nil {
			result[objID] = createObjectBatchObjResult{
				Error: fmt.Sprintf("check if object(%s) exists failed, err: %v", objID, err),
			}
			hasError = true
			continue
		}
		if !isObjExists {
			result[objID] = createObjectBatchObjResult{Error: fmt.Sprintf("object (%s) does not exist", objID)}
			hasError = true
			continue
		}

		// get group name to property id map
		groupNames := make([]string, 0)
		for _, attr := range inputData.Attr {
			if len(attr.PropertyGroupName) == 0 {
				continue
			}
			groupNames = append(groupNames, attr.PropertyGroupName)
		}

		grpNameIDMap := make(map[string]string)
		if len(groupNames) > 0 {
			grpCond := metadata.QueryCondition{
				Condition: mapstr.MapStr{
					metadata.GroupFieldGroupName: mapstr.MapStr{common.BKDBIN: groupNames},
					metadata.GroupFieldObjectID:  objID,
				},
				Fields: []string{metadata.GroupFieldGroupID, metadata.GroupFieldGroupName},
				Page:   metadata.BasePage{Limit: common.BKNoLimit},
			}

			grpRsp, err := a.clientSet.CoreService().Model().ReadAttributeGroup(kit.Ctx, kit.Header, objID, grpCond)
			if err != nil {
				result[objID] = createObjectBatchObjResult{
					Error: fmt.Sprintf("find object group failed, err: %v, cond: %#v", err, grpCond),
				}
				hasError = true
				continue
			}

			for _, grp := range grpRsp.Info {
				grpNameIDMap[grp.GroupName] = grp.GroupID
			}
		}

		// upsert the object's attribute
		result[objID], hasError = a.upsertObjectAttrBatch(kit, objID, inputData.Attr, grpNameIDMap)
	}

	if hasError {
		return result, kit.CCError.Error(common.CCErrCommNotAllSuccess)
	}
	return result, nil
}

func (a *attribute) upsertObjectAttrBatch(kit *rest.Kit, objID string, attributes map[int64]metadata.Attribute,
	grpNameIDMap map[string]string) (createObjectBatchObjResult, bool) {

	objRes := createObjectBatchObjResult{}
	hasError := false
	for idx, attr := range attributes {
		propID := attr.PropertyID
		if propID == common.BKInstParentStr {
			continue
		}

		attr.OwnerID = kit.SupplierAccount
		attr.ObjectID = objID
		if err := a.isValid(kit, true, &attr); err != nil {
			blog.Errorf("attribute(%#v) is invalid, err: %v, rid: %s", attr, err, kit.Rid)
			objRes.Errors = append(objRes.Errors, rowInfo{Row: idx, Info: err.Error(), PropID: propID})
			hasError = true
			continue
		}

		if len(attr.PropertyGroupName) != 0 {
			groupID, exists := grpNameIDMap[attr.PropertyGroupName]
			if exists {
				attr.PropertyGroup = groupID
			} else {
				grp := metadata.CreateModelAttributeGroup{
					Data: metadata.Group{GroupName: attr.PropertyGroupName, GroupID: NewGroupID(false), ObjectID: objID,
						OwnerID: kit.SupplierAccount, BizID: attr.BizID,
					}}

				_, err := a.clientSet.CoreService().Model().CreateAttributeGroup(kit.Ctx, kit.Header, objID, grp)
				if err != nil {
					blog.Errorf("create attribute group[%#v] failed, err: %v, rid: %s", grp, err, kit.Rid)
					objRes.Errors = append(objRes.Errors, rowInfo{Row: idx, Info: err.Error(), PropID: propID})
					hasError = true
					continue
				}
				attr.PropertyGroup = grp.Data.GroupID
			}
		} else {
			attr.PropertyGroup = NewGroupID(true)
		}

		// check if attribute exists, if exists, update these attributes, otherwise, create the attribute
		attrCond := mapstr.MapStr{metadata.AttributeFieldObjectID: objID, metadata.AttributeFieldPropertyID: propID}
		util.AddModelBizIDCondition(attrCond, attr.BizID)

		attrCnt, err := a.clientSet.CoreService().Count().GetCountByFilter(kit.Ctx, kit.Header,
			common.BKTableNameObjAttDes, []map[string]interface{}{attrCond})
		if err != nil {
			blog.Errorf("count attribute failed, err: %v, cond: %#v, rid: %s", err, attrCond, kit.Rid)
			objRes.Errors = append(objRes.Errors, rowInfo{Row: idx, Info: err.Error(), PropID: propID})
			hasError = true
			continue
		}

		if attrCnt[0] == 0 {
			// create attribute
			createAttrOpt := &metadata.CreateModelAttributes{Attributes: []metadata.Attribute{attr}}
			_, err := a.clientSet.CoreService().Model().CreateModelAttrs(kit.Ctx, kit.Header, objID, createAttrOpt)
			if err != nil {
				blog.Errorf("create attribute(%#v) failed, ObjID: %s, err: %v, rid: %s", attr, objID, err, kit.Rid)
				objRes.InsertFailed = append(objRes.InsertFailed, rowInfo{Row: idx, Info: err.Error(), PropID: propID})
				hasError = true
				continue
			}
		} else {
			// update attribute
			updateData := attr.ToMapStr()
			updateData.Remove(metadata.AttributeFieldPropertyID)
			updateData.Remove(metadata.AttributeFieldObjectID)
			updateData.Remove(metadata.AttributeFieldID)
			updateAttrOpt := metadata.UpdateOption{Condition: attrCond, Data: updateData}
			_, err := a.clientSet.CoreService().Model().UpdateModelAttrs(kit.Ctx, kit.Header, objID, &updateAttrOpt)
			if err != nil {
				blog.Errorf("failed to update module attr, err: %s, rid: %s", err, kit.Rid)
				objRes.UpdateFailed = append(objRes.UpdateFailed, rowInfo{Row: idx, Info: err.Error(), PropID: propID})
				hasError = true
				continue
			}
		}

		objRes.Success = append(objRes.Success, rowInfo{Row: idx, PropID: attr.PropertyID})
	}

	return objRes, hasError
}

// FindObjectBatch find object to attribute mapping batch
func (a *attribute) FindObjectBatch(kit *rest.Kit, objIDs []string) (mapstr.MapStr, error) {
	result := mapstr.New()

	for _, objID := range objIDs {
		attrCond := &metadata.QueryCondition{
			Condition: mapstr.MapStr{
				metadata.AttributeFieldObjectID: objID,
				metadata.AttributeFieldIsSystem: false,
				metadata.AttributeFieldIsAPI:    false,
				common.BKAppIDField:             0,
			},
			Page: metadata.BasePage{Limit: common.BKNoLimit},
		}
		attrRsp, err := a.clientSet.CoreService().Model().ReadModelAttr(kit.Ctx, kit.Header, objID, attrCond)
		if err != nil {
			blog.Errorf("get object(%s) not inner attributes failed, err: %v, rid: %s", err, kit.Rid)
			return nil, err
		}

		if len(attrRsp.Info) == 0 {
			result.Set(objID, mapstr.MapStr{"attr": attrRsp.Info})
			continue
		}

		groupIDs := make([]string, 0)
		for _, attr := range attrRsp.Info {
			groupIDs = append(groupIDs, attr.PropertyGroup)
		}

		grpCond := metadata.QueryCondition{
			Condition: mapstr.MapStr{
				metadata.GroupFieldGroupID:  mapstr.MapStr{common.BKDBIN: groupIDs},
				metadata.GroupFieldObjectID: objID,
			},
			Fields: []string{metadata.GroupFieldGroupID, metadata.GroupFieldGroupName},
			Page:   metadata.BasePage{Limit: common.BKNoLimit},
		}

		grpRsp, err := a.clientSet.CoreService().Model().ReadAttributeGroup(kit.Ctx, kit.Header, objID, grpCond)
		if err != nil {
			blog.Errorf("find object group failed, err: %v, cond: %#v, rid: %s", err, grpCond, kit.Rid)
			return nil, err
		}

		grpIDNameMap := make(map[string]string)
		for _, grp := range grpRsp.Info {
			grpIDNameMap[grp.GroupID] = grp.GroupName
		}

		for idx, attr := range attrRsp.Info {
			attrRsp.Info[idx].PropertyGroupName = grpIDNameMap[attr.PropertyGroup]
		}

		result.Set(objID, mapstr.MapStr{"attr": attrRsp.Info})
	}

	return result, nil
}

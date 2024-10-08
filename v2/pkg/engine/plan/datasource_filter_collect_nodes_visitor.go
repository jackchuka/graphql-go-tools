package plan

import (
	"fmt"
	"slices"

	"github.com/kingledion/go-tools/tree"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

type nodesCollector struct {
	operation   *ast.Document
	definition  *ast.Document
	dataSources []DataSource
	nodes       *NodeSuggestions
	report      *operationreport.Report
}

func (c *nodesCollector) CollectNodes() *NodeSuggestions {
	c.buildTree()
	if c.report.HasErrors() {
		return nil
	}

	c.collectNodes()
	if c.report.HasErrors() {
		return nil
	}

	return c.nodes
}

func (c *nodesCollector) collectNodes() {
	walker := astvisitor.NewWalker(32)
	visitor := &collectNodesVisitor{
		operation:  c.operation,
		definition: c.definition,
		walker:     &walker,
		nodes:      c.nodes,
	}
	walker.RegisterFieldVisitor(visitor)

	for _, dataSource := range c.dataSources {
		visitor.dataSource = dataSource
		visitor.keyPaths = make(map[string]struct{})
		walker.Walk(c.operation, c.definition, c.report)
		if c.report.HasErrors() {
			return
		}
	}
}

func (c *nodesCollector) buildTree() {
	walker := astvisitor.NewWalker(32)
	visitor := &treeBuilderVisitor{
		operation:  c.operation,
		definition: c.definition,
		walker:     &walker,
		nodes:      c.nodes,
	}
	walker.RegisterEnterDocumentVisitor(visitor)
	walker.RegisterFieldVisitor(visitor)
	walker.Walk(c.operation, c.definition, c.report)
}

type treeBuilderVisitor struct {
	walker        *astvisitor.Walker
	operation     *ast.Document
	definition    *ast.Document
	nodes         *NodeSuggestions
	parentNodeIds []uint
}

func (f *treeBuilderVisitor) EnterDocument(_, _ *ast.Document) {
	f.parentNodeIds = []uint{treeRootID}
}

func (f *treeBuilderVisitor) EnterField(ref int) {
	if f.nodes.IsFieldSeen(ref) {
		currentNodeId := TreeNodeID(ref)
		f.parentNodeIds = append(f.parentNodeIds, currentNodeId)
		return
	}
	f.nodes.AddSeenField(ref)

	parentNodeId := f.currentParentID()
	currentNodeId := TreeNodeID(ref)

	// we intentionally ignore the return values added, exists
	// because we do not recheck the same field refs, so all added nodes should be new and unique
	_, _ = f.nodes.responseTree.Add(currentNodeId, parentNodeId, nil)
	f.parentNodeIds = append(f.parentNodeIds, currentNodeId)
}

func (f *treeBuilderVisitor) currentParentID() uint {
	return f.parentNodeIds[len(f.parentNodeIds)-1]
}

func (f *treeBuilderVisitor) LeaveField(ref int) {
	parentNodeId := f.currentParentID()
	currentNodeId := TreeNodeID(ref)

	if parentNodeId == currentNodeId {
		f.parentNodeIds = f.parentNodeIds[:len(f.parentNodeIds)-1]
	}
}

type collectNodesVisitor struct {
	walker     *astvisitor.Walker
	operation  *ast.Document
	definition *ast.Document
	dataSource DataSource
	nodes      *NodeSuggestions

	keyPaths map[string]struct{}
}

func (f *collectNodesVisitor) hasSuggestionForField(itemIds []int, ref int) bool {
	return slices.ContainsFunc(itemIds, func(i int) bool {
		suggestion := f.nodes.items[i]
		return suggestion.FieldRef == ref && suggestion.DataSourceHash == f.dataSource.Hash()
	})
}

func (f *collectNodesVisitor) hasProvidesConfiguration(typeName, fieldName string) (selectionSet string, ok bool) {
	providesIdx := slices.IndexFunc(f.dataSource.FederationConfiguration().Provides, func(provide FederationFieldConfiguration) bool {
		return provide.TypeName == typeName && provide.FieldName == fieldName
	})
	if providesIdx == -1 {
		return "", false
	}
	return f.dataSource.FederationConfiguration().Provides[providesIdx].SelectionSet, true
}

func (f *collectNodesVisitor) isEntityInterface(typeName string) bool {
	cfg := f.dataSource.FederationConfiguration()
	return cfg.HasEntityInterface(typeName)
}

func (f *collectNodesVisitor) isInterfaceObject(typeName string) bool {
	cfg := f.dataSource.FederationConfiguration()
	return cfg.HasInterfaceObject(typeName)
}

// has disabled entity resolver
func (f *collectNodesVisitor) allKeysHasDisabledEntityResolver(typeName string) bool {
	keys := f.dataSource.FederationConfiguration().Keys
	return !slices.ContainsFunc(keys.FilterByTypeAndResolvability(typeName, false), func(k FederationFieldConfiguration) bool {
		return !k.DisableEntityResolver
	})
}

func (f *collectNodesVisitor) handleProvidesSuggestions(fieldRef int, typeName, fieldName, currentPath string) {
	if !f.operation.FieldHasSelections(fieldRef) {
		return
	}

	providesSelectionSet, hasProvides := f.hasProvidesConfiguration(typeName, fieldName)
	if !hasProvides {
		return
	}

	if f.walker.EnclosingTypeDefinition.Kind != ast.NodeKindObjectTypeDefinition {
		return
	}

	fieldDefRef, ok := f.definition.ObjectTypeDefinitionFieldWithName(f.walker.EnclosingTypeDefinition.Ref, f.operation.FieldNameBytes(fieldRef))
	if !ok {
		return
	}
	fieldTypeName := f.definition.FieldDefinitionTypeNameString(fieldDefRef)

	providesFieldSet, report := providesFragment(fieldTypeName, providesSelectionSet, f.definition)
	if report.HasErrors() {
		f.walker.StopWithInternalErr(fmt.Errorf("failed to parse provides fields for %s.%s at path %s: %v", typeName, fieldName, currentPath, report))
		return
	}

	selectionSetRef, ok := f.operation.FieldSelectionSet(fieldRef)
	if !ok {
		f.walker.StopWithInternalErr(fmt.Errorf("failed to get selection set ref for %s.%s at path %s. Field with provides directive should have a selections", typeName, fieldName, currentPath))
		return
	}

	input := &providesInput{
		providesFieldSet:      providesFieldSet,
		operation:             f.operation,
		definition:            f.definition,
		operationSelectionSet: selectionSetRef,
		report:                report,
		parentPath:            currentPath,
		dataSourceHash:        f.dataSource.Hash(),
		dataSourceID:          f.dataSource.Id(),
		dataSourceName:        f.dataSource.Name(),
	}
	suggestions := providesSuggestions(input)
	if report.HasErrors() {
		f.walker.StopWithInternalErr(fmt.Errorf("failed to get provides suggestions for %s.%s at path %s: %v", typeName, fieldName, currentPath, report))
		return
	}

	for _, suggestion := range suggestions {
		nodeID := TreeNodeID(suggestion.FieldRef)
		treeNode, _ := f.nodes.responseTree.Find(nodeID)

		nodesIndexes := treeNode.GetData()

		exists := false
		for _, idx := range nodesIndexes {
			if f.nodes.items[idx].DataSourceHash == f.dataSource.Hash() {
				f.nodes.items[idx].IsProvided = true
				exists = true
			}
		}
		if exists {
			continue
		}

		// if suggestions is not exists we adding it
		suggestionIdx := f.nodes.addSuggestion(suggestion)
		nodesIndexes = append(nodesIndexes, suggestionIdx)
		treeNode.SetData(nodesIndexes)
	}
}

func (f *collectNodesVisitor) shouldAddUnionTypenameFieldSuggestion(treeNode tree.Node[[]int]) bool {
	if f.walker.EnclosingTypeDefinition.Kind != ast.NodeKindUnionTypeDefinition {
		return false
	}

	parent := treeNode.GetParent()
	parentItems := parent.GetData()

	for _, idx := range parentItems {
		if f.nodes.items[idx].DataSourceHash == f.dataSource.Hash() {
			return true
		}
	}

	return false
}

func (f *collectNodesVisitor) EnterField(fieldRef int) {
	typeName := f.walker.EnclosingTypeDefinition.NameString(f.definition)
	fieldName := f.operation.FieldNameUnsafeString(fieldRef)
	fieldAliasOrName := f.operation.FieldAliasOrNameString(fieldRef)

	isTypeName := fieldName == typeNameField
	parentPath := f.walker.Path.DotDelimitedString()
	onFragment := f.walker.Path.EndsWithFragment()
	var parentPathWithoutFragment *string
	if onFragment {
		p := f.walker.Path[:len(f.walker.Path)-1].DotDelimitedString()
		parentPathWithoutFragment = &p
	}
	currentPath := parentPath + "." + fieldAliasOrName

	f.handleProvidesSuggestions(fieldRef, typeName, fieldName, currentPath)

	if isTypeName && f.isInterfaceObject(typeName) {
		// we should not add a typename on the interface object
		// to not select it during node suggestions calculation
		// we will add a typename field to the interface object query in the datasource planner

		// at the same type we should allow to select a typename on the entity interface
		return
	}

	// hasRootNode is true when:
	// - ds config has a root node for the field
	// - we have a root node with typename and the field is a __typename field
	// - the field is a root query type (query, mutation) and the field is a __typename field
	hasRootNode := f.dataSource.HasRootNode(typeName, fieldName) || (isTypeName && (f.dataSource.HasRootNodeWithTypename(typeName) || IsMutationOrQueryRootType(typeName)))

	// hasChildNode is true when:
	// - ds config has a child node for the field
	// - we have a child node with typename and the field is a __typename field
	// - the field is __typename field on a union, and we have a suggestion for the parent field
	hasChildNode := f.dataSource.HasChildNode(typeName, fieldName) || (isTypeName && f.dataSource.HasChildNodeWithTypename(typeName))

	// external root node is a node having external directive, to be resolvable it needs to be provided or be part of a key
	// So the node will not be external if it is mentioned in both fields and external fields
	isExternalRootNode := f.dataSource.HasExternalRootNode(typeName, fieldName) && !hasRootNode
	isExternalChildNode := f.dataSource.HasExternalChildNode(typeName, fieldName) && !hasChildNode
	isExternal := isExternalRootNode || isExternalChildNode

	currentNodeId := TreeNodeID(fieldRef)
	treeNode, _ := f.nodes.responseTree.Find(currentNodeId)
	itemIds := treeNode.GetData()

	hasChildNode = hasChildNode || f.shouldAddUnionTypenameFieldSuggestion(treeNode)
	hasSelections := f.operation.FieldHasSelections(fieldRef)

	if f.hasSuggestionForField(itemIds, fieldRef) {
		for _, idx := range itemIds {
			if f.nodes.items[idx].DataSourceHash == f.dataSource.Hash() {
				f.nodes.items[idx].IsExternal = isExternal

				// we need to also set it here, because provided nodes do not have this property yet
				f.nodes.items[idx].IsLeaf = !hasSelections
			}
		}

		return
	}

	if hasRootNode || hasChildNode || isExternal {
		disabledEntityResolver := hasRootNode && f.allKeysHasDisabledEntityResolver(typeName)

		node := NodeSuggestion{
			TypeName:                  typeName,
			FieldName:                 fieldName,
			DataSourceHash:            f.dataSource.Hash(),
			DataSourceID:              f.dataSource.Id(),
			DataSourceName:            f.dataSource.Name(),
			Path:                      currentPath,
			ParentPath:                parentPath,
			IsRootNode:                hasRootNode,
			onFragment:                onFragment,
			parentPathWithoutFragment: parentPathWithoutFragment,
			FieldRef:                  fieldRef,
			DisabledEntityResolver:    disabledEntityResolver,
			IsEntityInterfaceTypeName: isTypeName && f.isEntityInterface(typeName),
			IsExternal:                isExternal,
			IsLeaf:                    !hasSelections,
		}

		f.nodes.addSuggestion(&node)
		itemId := len(f.nodes.items) - 1

		treeNode, _ := f.nodes.responseTree.Find(currentNodeId)
		itemIds := treeNode.GetData()
		itemIds = append(itemIds, itemId)
		treeNode.SetData(itemIds)
	}
}

func (f *collectNodesVisitor) LeaveField(ref int) {

}

func TreeNodeID(fieldRef int) uint {
	// we add 100 to the fieldRef to make sure that the tree node id is never 0
	// cause 0 is a valid field ref
	// but for tree 0 is reserved for the root node
	return uint(100 + fieldRef)
}

const (
	queryTypeName    = "Query"
	mutationTypeName = "Mutation"
)

func IsMutationOrQueryRootType(typeName string) bool {
	return queryTypeName == typeName || mutationTypeName == typeName
}

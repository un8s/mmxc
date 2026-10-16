package mmxc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	statementDocumentDirName  = "_documents"
	statementExtractionSchema = 2
)

// statementSection is one independently reconcilable account section in an
// uploaded document. Candidates are owned by the section, not inferred from
// the MMEX account to which the section is later attached.
type statementSection struct {
	ID         string            `json:"id"`
	Header     statementHeader   `json:"header"`
	Candidates []importCandidate `json:"candidates"`
	RawJSON    string            `json:"raw_json,omitempty"`
	Ignored    bool              `json:"ignored,omitempty"`
}

// statementDocument is the content-addressed extraction cache for one uploaded
// blob. Account directories contain small archivedStatement projections that
// point at one of these sections, allowing the same PDF to serve many accounts.
type statementDocument struct {
	SchemaVersion int                `json:"schema_version"`
	ContentHash   string             `json:"content_hash"`
	File          string             `json:"file"`
	OriginalName  string             `json:"original_name"`
	UploadedAt    string             `json:"uploaded_at"`
	RawJSON       string             `json:"raw_json,omitempty"`
	Sections      []statementSection `json:"sections"`
}

func validStatementHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func (a *app) statementDocumentsDir() string {
	return filepath.Join(a.config.StatementDir, statementDocumentDirName)
}

func (a *app) statementDocumentDir(hash string) string {
	if !validStatementHash(hash) {
		return ""
	}
	return filepath.Join(a.statementDocumentsDir(), hash)
}

func (a *app) statementDocumentManifestPath(hash string) string {
	dir := a.statementDocumentDir(hash)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "manifest.json")
}

func (a *app) statementDocumentBlobPath(hash, file string) string {
	dir := a.statementDocumentDir(hash)
	if dir == "" || file == "" || filepath.Base(file) != file {
		return ""
	}
	return filepath.Join(dir, file)
}

func normalizeStatementSections(sections []statementSection, uploaded time.Time) []statementSection {
	out := make([]statementSection, 0, len(sections))
	used := map[string]bool{}
	for i := range sections {
		section := sections[i]
		section.Header.AccountName = strings.TrimSpace(section.Header.AccountName)
		section.Header.AccountNumber = strings.TrimSpace(section.Header.AccountNumber)
		section.Header.ClosingDate = normalizeISODate(section.Header.ClosingDate)
		section.Header.PeriodStart, section.Header.PeriodEnd = resolveStatementPeriod(section.Header, uploaded)
		section.ID = statementSectionID(section.Header, i)
		for used[section.ID] {
			section.ID = statementSectionID(section.Header, i+len(out)+1)
		}
		used[section.ID] = true
		if section.Candidates == nil {
			section.Candidates = []importCandidate{}
		}
		if section.RawJSON == "" {
			if encoded, err := json.MarshalIndent(section.Candidates, "", "  "); err == nil {
				section.RawJSON = string(encoded)
			}
		}
		out = append(out, section)
	}
	return out
}

func statementSectionID(header statementHeader, index int) string {
	identity := strings.ToLower(strings.Join([]string{
		strings.Join(strings.Fields(header.AccountName), " "),
		strings.Join(strings.Fields(header.AccountNumber), ""),
		strconv.Itoa(index),
	}, "|"))
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:6])
}

func statementAttachmentName(doc statementDocument, section statementSection) string {
	ext := strings.ToLower(filepath.Ext(doc.OriginalName))
	if ext == "" {
		ext = ".statement"
	}
	return doc.ContentHash[:12] + "-" + section.ID + ext
}

func (a *app) saveStatementDocument(originalName string, data []byte, sections []statementSection, rawJSON string) (statementDocument, error) {
	stamp := time.Now()
	hash := statementContentHash(data)
	dir := a.statementDocumentDir(hash)
	if dir == "" {
		return statementDocument{}, errors.New("invalid statement content hash")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return statementDocument{}, err
	}
	ext := strings.ToLower(filepath.Ext(originalName))
	if ext == "" || len(ext) > 10 {
		ext = ".statement"
	}
	blobName := "document" + ext
	blobPath := filepath.Join(dir, blobName)
	if _, err := os.Stat(blobPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(blobPath, data, 0o600); err != nil {
			return statementDocument{}, err
		}
	} else if err != nil {
		return statementDocument{}, err
	}
	doc := statementDocument{
		SchemaVersion: statementExtractionSchema,
		ContentHash:   hash,
		File:          blobName,
		OriginalName:  filepath.Base(originalName),
		UploadedAt:    stamp.Format(time.RFC3339),
		RawJSON:       strings.TrimSpace(rawJSON),
		Sections:      normalizeStatementSections(sections, stamp),
	}
	if len(doc.Sections) == 0 {
		return statementDocument{}, errors.New("statement extraction returned no account sections")
	}
	if err := a.writeStatementDocument(doc); err != nil {
		return statementDocument{}, err
	}
	return doc, nil
}

func (a *app) writeStatementDocument(doc statementDocument) error {
	path := a.statementDocumentManifestPath(doc.ContentHash)
	if path == "" {
		return errors.New("invalid statement document path")
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (a *app) loadStatementDocument(hash string) (statementDocument, error) {
	path := a.statementDocumentManifestPath(hash)
	if path == "" {
		return statementDocument{}, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return statementDocument{}, err
	}
	var doc statementDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return statementDocument{}, err
	}
	if doc.SchemaVersion != statementExtractionSchema || doc.ContentHash != hash || len(doc.Sections) == 0 {
		return statementDocument{}, fmt.Errorf("unsupported statement extraction cache version")
	}
	return doc, nil
}

func (a *app) documentStatementAttachments(hash string) []archivedStatement {
	var out []archivedStatement
	for _, meta := range a.allArchivedStatements() {
		if meta.DocumentHash == hash {
			out = append(out, meta)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SectionID == out[j].SectionID {
			return out[i].Confirmed && !out[j].Confirmed
		}
		return out[i].SectionID < out[j].SectionID
	})
	return out
}

func (a *app) findStatementSectionAttachment(hash, sectionID string) (archivedStatement, bool) {
	for _, meta := range a.documentStatementAttachments(hash) {
		if meta.SectionID == sectionID {
			return meta, true
		}
	}
	return archivedStatement{}, false
}

func (a *app) archiveStatementDocument(originalName string, data []byte, sections []statementSection, rawJSON string) (statementDocument, []archivedStatement, error) {
	doc, err := a.saveStatementDocument(originalName, data, sections, rawJSON)
	if err != nil {
		return statementDocument{}, nil, err
	}
	attachments, err := a.ensureStatementDocumentAttachments(doc)
	if err != nil {
		return statementDocument{}, nil, err
	}
	return doc, attachments, nil
}

// ensureStatementDocumentAttachments creates one account projection per
// extracted section. Existing projections win, so a cached re-upload never
// overwrites a user's confirmed assignment.
func (a *app) ensureStatementDocumentAttachments(doc statementDocument) ([]archivedStatement, error) {
	existing := a.documentStatementAttachments(doc.ContentHash)
	bySection := map[string]archivedStatement{}
	for _, meta := range existing {
		if previous, ok := bySection[meta.SectionID]; !ok || (!previous.Confirmed && meta.Confirmed) {
			bySection[meta.SectionID] = meta
		}
	}
	out := make([]archivedStatement, 0, len(doc.Sections))
	for _, section := range doc.Sections {
		if section.Ignored {
			continue
		}
		if meta, ok := bySection[section.ID]; ok {
			out = append(out, meta)
			continue
		}
		meta, err := a.createStatementSectionAttachment(doc, section)
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, nil
}

func (a *app) createStatementSectionAttachment(doc statementDocument, section statementSection) (archivedStatement, error) {
	accountID, confirmed := a.detectStatementAccount(section.Header)
	if accountID <= 0 {
		accountID = a.firstReviewAccountID()
		confirmed = false
	}
	if len(doc.Sections) > 1 {
		// A multi-account upload always gets one explicit mapping review before
		// any section contributes coverage or import candidates. Detection is a
		// preselection only, even when a learned mapping is available.
		confirmed = false
	}
	meta := archivedStatement{
		File:           statementAttachmentName(doc, section),
		OriginalName:   doc.OriginalName,
		UploadedAt:     doc.UploadedAt,
		AccountID:      accountID,
		ContentHash:    doc.ContentHash,
		DocumentHash:   doc.ContentHash,
		DocumentFile:   doc.File,
		SectionID:      section.ID,
		Confirmed:      confirmed,
		PeriodStart:    section.Header.PeriodStart,
		PeriodEnd:      section.Header.PeriodEnd,
		AccountName:    section.Header.AccountName,
		AccountNumber:  section.Header.AccountNumber,
		ClosingDate:    section.Header.ClosingDate,
		ClosingBalance: section.Header.ClosingBalance,
		HasClosing:     section.Header.HasClosing,
		Candidates:     section.Candidates,
		RawJSON:        section.RawJSON,
	}
	if err := a.writeStatementSidecar(&meta); err != nil {
		return archivedStatement{}, err
	}
	return meta, nil
}

// deleteStatementDocument removes every account projection and then the shared
// manifest/blob. The hash-derived directory is validated before any removal.
func (a *app) deleteStatementDocument(doc statementDocument) error {
	if !validStatementHash(doc.ContentHash) {
		return errors.New("invalid statement document hash")
	}
	for _, meta := range a.documentStatementAttachments(doc.ContentHash) {
		if err := a.deleteArchivedStatement(meta); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, path := range []string{
		a.statementDocumentBlobPath(doc.ContentHash, doc.File),
		a.statementDocumentManifestPath(doc.ContentHash),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(a.statementDocumentDir(doc.ContentHash))
}

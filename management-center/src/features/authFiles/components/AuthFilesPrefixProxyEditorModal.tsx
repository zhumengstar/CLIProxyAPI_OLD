import { useTranslation } from 'react-i18next';
import { Modal } from '@/components/ui/Modal';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Input } from '@/components/ui/Input';
import type {
  PrefixProxyEditorField,
  PrefixProxyEditorFieldValue,
  PrefixProxyEditorState,
} from '@/features/authFiles/hooks/useAuthFilesPrefixProxyEditor';
import styles from '@/pages/AuthFilesPage.module.scss';

export type AuthFilesPrefixProxyEditorModalProps = {
  disableControls: boolean;
  editor: PrefixProxyEditorState | null;
  updatedText: string;
  dirty: boolean;
  onClose: () => void;
  onCopyText: (text: string) => void | Promise<void>;
  onSave: () => void;
  onChange: (field: PrefixProxyEditorField, value: PrefixProxyEditorFieldValue) => void;
};

export function AuthFilesPrefixProxyEditorModal(props: AuthFilesPrefixProxyEditorModalProps) {
  const { t } = useTranslation();
  const { disableControls, editor, updatedText, dirty, onClose, onCopyText, onSave, onChange } =
    props;
  const formatJsonText = (text: string) => {
    if (!text) return '';
    try {
      return JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      return text;
    }
  };
  const previewText = formatJsonText(updatedText);

  return (
    <Modal
      open={Boolean(editor)}
      onClose={onClose}
      closeDisabled={editor?.saving === true}
      width={720}
      title={
        editor?.fileName
          ? t('auth_files.auth_field_editor_title', { name: editor.fileName })
          : t('auth_files.prefix_proxy_button')
      }
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={editor?.saving === true}>
            {dirty ? t('common.cancel') : t('common.close')}
          </Button>
          <Button
            variant="secondary"
            onClick={() => {
              if (!updatedText) return;
              void onCopyText(updatedText);
            }}
            disabled={editor?.saving === true || !updatedText}
          >
            {t('common.copy')}
          </Button>
          <Button
            onClick={onSave}
            loading={editor?.saving === true}
            disabled={
              disableControls ||
              editor?.saving === true ||
              !dirty ||
              !editor?.json ||
              Boolean(editor?.headersTouched && editor.headersError)
            }
          >
            {t('common.save')}
          </Button>
        </>
      }
    >
      {editor && (
        <div className={styles.prefixProxyEditor}>
          {editor.loading ? (
            <div className={styles.prefixProxyLoading}>
              <LoadingSpinner size={14} />
              <span>{t('auth_files.prefix_proxy_loading')}</span>
            </div>
          ) : (
            <>
              {editor.error && <div className={styles.prefixProxyError}>{editor.error}</div>}
              <div className={styles.prefixProxyJsonWrapper}>
                <label className={styles.prefixProxyLabel}>
                  {t('auth_files.prefix_proxy_info_label')}
                </label>
                <textarea
                  className={styles.prefixProxyTextarea}
                  rows={8}
                  readOnly
                  value={editor.fileInfoText}
                />
              </div>
              <div className={styles.prefixProxyJsonWrapper}>
                <label className={styles.prefixProxyLabel}>
                  {t('auth_files.prefix_proxy_source_label')}
                </label>
                <textarea
                  className={styles.prefixProxyTextarea}
                  rows={10}
                  readOnly
                  value={previewText}
                />
              </div>
              <div className={styles.prefixProxyFields}>
                <Input
                  label={t('auth_files.prefix_label')}
                  value={editor.prefix}
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('prefix', e.target.value)}
                />
                <Input
                  label={t('auth_files.proxy_url_label')}
                  value={editor.proxyUrl}
                  placeholder={t('auth_files.proxy_url_placeholder')}
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('proxyUrl', e.target.value)}
                />
                <Input
                  label={t('auth_files.priority_label')}
                  value={editor.priority}
                  placeholder={t('auth_files.priority_placeholder')}
                  hint={t('auth_files.priority_hint')}
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('priority', e.target.value)}
                />
                <Input
                  label="账号成本"
                  type="number"
                  step="0.001"
                  min="0"
                  inputMode="decimal"
                  value={editor.accountCost}
                  placeholder="例如 0.15"
                  hint="按人民币/账号记录；留空或 0 表示不设置"
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('accountCost', e.target.value)}
                />
                <Input
                  label="渠道来源"
                  value={editor.sourceChannel}
                  placeholder="例如 plus / 供应商A / 渠道1"
                  hint="账号来源备注，可自由填写"
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('sourceChannel', e.target.value)}
                />
                <Input
                  label="计时开始"
                  value={editor.accountStartedAt}
                  placeholder="例如 2026-05-25T12:00:00Z"
                  hint="用于计算账号存活时间；留空则回退到注册/导入时间"
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('accountStartedAt', e.target.value)}
                />
                <div className="form-group">
                  <label>{t('auth_files.headers_label')}</label>
                  <textarea
                    className={`input ${editor.headersError ? styles.prefixProxyTextareaInvalid : ''}`}
                    value={editor.headersText}
                    placeholder={t('auth_files.headers_placeholder')}
                    rows={4}
                    aria-invalid={Boolean(editor.headersError)}
                    disabled={disableControls || editor.saving || !editor.json}
                    onChange={(e) => onChange('headersText', e.target.value)}
                  />
                  {editor.headersError && <div className="error-box">{editor.headersError}</div>}
                  <div className="hint">{t('auth_files.headers_hint')}</div>
                </div>
                <Input
                  label={t('auth_files.note_label')}
                  value={editor.note}
                  placeholder={t('auth_files.note_placeholder')}
                  hint={t('auth_files.note_hint')}
                  disabled={disableControls || editor.saving || !editor.json}
                  onChange={(e) => onChange('note', e.target.value)}
                />
              </div>
            </>
          )}
        </div>
      )}
    </Modal>
  );
}

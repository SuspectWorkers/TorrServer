import axios from 'axios'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import Button from '@material-ui/core/Button'
import CircularProgress from '@material-ui/core/CircularProgress'
import ListItemIcon from '@material-ui/core/ListItemIcon'
import ListItemText from '@material-ui/core/ListItemText'
import useMediaQuery from '@material-ui/core/useMediaQuery'
import DescriptionIcon from '@material-ui/icons/Description'
import GetAppIcon from '@material-ui/icons/GetApp'
import RefreshIcon from '@material-ui/icons/Refresh'

import { logsHost } from 'utils/Hosts'
import { isStandaloneApp } from 'utils/Utils'
import { StyledDialog, StyledMenuButtonWrapper } from 'style/CustomMaterialUiStyles'
import useOnStandaloneAppOutsideClick from 'utils/useOnStandaloneAppOutsideClick'

export default function LogsDialog({ isOffline, isLoading }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [logsText, setLogsText] = useState('')
  const [fetching, setFetching] = useState(false)
  const [downloading, setDownloading] = useState(false)
  const [errorMsg, setErrorMsg] = useState('')
  const fullScreen = useMediaQuery('@media (max-width:930px)')
  const preRef = useRef(null)

  const onClose = () => setOpen(false)
  const ref = useOnStandaloneAppOutsideClick(onClose)

  const loadLogs = useCallback(async () => {
    setFetching(true)
    setErrorMsg('')
    try {
      const { data } = await axios.get(`${logsHost()}?tail=262144`, { responseType: 'text' })
      setLogsText(typeof data === 'string' ? data : String(data || ''))
    } catch (err) {
      setErrorMsg(err?.message || t('LogsDialog.LoadFailed'))
    } finally {
      setFetching(false)
    }
  }, [t])

  useEffect(() => {
    if (open) {
      loadLogs()
    }
  }, [open, loadLogs])

  useEffect(() => {
    if (open && preRef.current) {
      preRef.current.scrollTop = preRef.current.scrollHeight
    }
  }, [open, logsText])

  const handleDownload = async () => {
    setDownloading(true)
    setErrorMsg('')
    try {
      const response = await axios.get(`${logsHost()}?download=1&diag=1`, { responseType: 'blob' })
      const disposition = response.headers?.['content-disposition'] || ''
      const match = /filename="?([^";]+)"?/i.exec(disposition)
      const filename = match?.[1] || 'torrserver-debug.txt'
      const url = window.URL.createObjectURL(new Blob([response.data], { type: 'text/plain;charset=utf-8' }))
      const link = document.createElement('a')
      link.href = url
      link.setAttribute('download', filename)
      document.body.appendChild(link)
      link.click()
      link.remove()
      window.URL.revokeObjectURL(url)
    } catch (err) {
      setErrorMsg(err?.message || t('LogsDialog.DownloadFailed'))
    } finally {
      setDownloading(false)
    }
  }

  return (
    <>
      <StyledMenuButtonWrapper disabled={isOffline || isLoading} button onClick={() => setOpen(true)}>
        {isStandaloneApp ? (
          <>
            <DescriptionIcon />
            <div>{t('LogsDialog.MenuTitle')}</div>
          </>
        ) : (
          <>
            <ListItemIcon>
              <DescriptionIcon />
            </ListItemIcon>
            <ListItemText primary={t('LogsDialog.MenuTitle')} />
          </>
        )}
      </StyledMenuButtonWrapper>

      <StyledDialog open={open} onClose={onClose} fullScreen={fullScreen} fullWidth maxWidth='md' ref={ref}>
        <div style={{ display: 'flex', flexDirection: 'column', height: fullScreen ? '100%' : '75vh', padding: 20 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <div style={{ fontSize: 20, fontWeight: 500 }}>{t('LogsDialog.Title')}</div>
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              <Button
                variant='outlined'
                color='primary'
                size='small'
                onClick={loadLogs}
                disabled={fetching}
                startIcon={fetching ? <CircularProgress size={16} /> : <RefreshIcon />}
              >
                {t('LogsDialog.Refresh')}
              </Button>
              <Button
                variant='contained'
                color='secondary'
                size='small'
                onClick={handleDownload}
                disabled={downloading}
                startIcon={downloading ? <CircularProgress size={16} /> : <GetAppIcon />}
              >
                {t('LogsDialog.DownloadReport')}
              </Button>
            </div>
          </div>

          <div style={{ fontSize: 12, opacity: 0.75, marginBottom: 10 }}>{t('LogsDialog.Hint')}</div>

          {errorMsg && <div style={{ color: '#f44336', marginBottom: 8, fontSize: 13 }}>{errorMsg}</div>}

          <pre
            ref={preRef}
            style={{
              flex: 1,
              margin: 0,
              padding: 12,
              overflow: 'auto',
              borderRadius: 6,
              background: '#11161b',
              color: '#d8dee9',
              fontSize: 12,
              lineHeight: 1.45,
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-word',
            }}
          >
            {logsText || (fetching ? t('LogsDialog.Loading') : t('LogsDialog.Empty'))}
          </pre>

          <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 14 }}>
            <Button onClick={onClose} color='primary' variant='contained'>
              {t('Close')}
            </Button>
          </div>
        </div>
      </StyledDialog>
    </>
  )
}

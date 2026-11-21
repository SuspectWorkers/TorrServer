import { useTranslation } from 'react-i18next'
import { USBIcon, RAMIcon } from 'icons'
import { Button, FormControlLabel, Switch } from '@material-ui/core'
import TextField from '@material-ui/core/TextField'

import {
  CacheLegendGrid,
  CacheLegendDot,
  MainSettingsContent,
  StorageButton,
  StorageIconWrapper,
  CacheStorageSelector,
  SettingSectionLabel,
  PreloadCachePercentage,
  cacheBeforeReaderColor,
  cacheAfterReaderColor,
} from './style'
import SliderInput from './SliderInput'

const CacheStorageLocationLabel = ({ style }) => {
  const { t } = useTranslation()

  return (
    <SettingSectionLabel style={style}>
      {t('SettingsDialog.CacheStorageLocation')}
      <small>{t('SettingsDialog.UseDiskDesc')}</small>
    </SettingSectionLabel>
  )
}

export default function PrimarySettingsComponent({
  settings,
  inputForm,
  cachePercentage,
  preloadCachePercentage,
  cacheSize,
  isProMode,
  setCacheSize,
  setCachePercentage,
  setPreloadCachePercentage,
  updateSettings,
}) {
  const { t } = useTranslation()
  const { UseDisk, TorrentsSavePath, RemoveCacheOnDrop } = settings || {}
  const preloadCacheSize = Math.round((cacheSize / 100) * preloadCachePercentage)

  const applyProfile = profile => {
    setCacheSize(profile.CacheSize)
    setCachePercentage(profile.ReaderReadAHead)
    setPreloadCachePercentage(profile.PreloadCache)
    updateSettings({
      CacheSize: profile.CacheSize,
      ReaderReadAHead: profile.ReaderReadAHead,
      PreloadCache: profile.PreloadCache,
      ConnectionsLimit: profile.ConnectionsLimit,
      PauseTimeout: profile.PauseTimeout,
    })
  }

  return (
    <MainSettingsContent>
      <div>
        <SettingSectionLabel>{t('SettingsDialog.Profiles.Title')}</SettingSectionLabel>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 18 }}>
          <Button
            size='small'
            variant='outlined'
            color='secondary'
            onClick={() =>
              applyProfile({
                CacheSize: 64,
                ReaderReadAHead: 95,
                PreloadCache: 50,
                ConnectionsLimit: 25,
                PauseTimeout: 180,
              })
            }
          >
            {t('SettingsDialog.Profiles.Standard')}
          </Button>
          <Button
            size='small'
            variant='outlined'
            color='secondary'
            onClick={() =>
              applyProfile({
                CacheSize: 256,
                ReaderReadAHead: 95,
                PreloadCache: 35,
                ConnectionsLimit: 35,
                PauseTimeout: 300,
              })
            }
          >
            {t('SettingsDialog.Profiles.Remux4K')}
          </Button>
          <Button
            size='small'
            variant='outlined'
            color='secondary'
            onClick={() =>
              applyProfile({
                CacheSize: 64,
                ReaderReadAHead: 90,
                PreloadCache: 25,
                ConnectionsLimit: 20,
                PauseTimeout: 300,
              })
            }
          >
            {t('SettingsDialog.Profiles.SmartTV')}
          </Button>
        </div>

        <SettingSectionLabel>{t('SettingsDialog.CacheSettings')}</SettingSectionLabel>

        <PreloadCachePercentage
          value={100 - cachePercentage}
          label={`${t('Cache')} ${cacheSize} ${t('MB')}`}
          preloadCachePercentage={preloadCachePercentage}
        />

        <CacheLegendGrid>
          <CacheLegendDot color={cacheBeforeReaderColor} aria-hidden />
          <div className='cache-legend-value'>
            {100 - cachePercentage}% ({Math.round((cacheSize / 100) * (100 - cachePercentage))} {t('MB')})
          </div>
          <div className='cache-legend-desc'>{t('SettingsDialog.CacheBeforeReaderDesc')}</div>

          <CacheLegendDot color={cacheAfterReaderColor} aria-hidden />
          <div className='cache-legend-value'>
            {cachePercentage}% ({Math.round((cacheSize / 100) * cachePercentage)} {t('MB')})
          </div>
          <div className='cache-legend-desc'>{t('SettingsDialog.CacheAfterReaderDesc')}</div>
        </CacheLegendGrid>

        <br />

        <SliderInput
          isProMode={isProMode}
          title={t('SettingsDialog.CacheSize')}
          value={cacheSize}
          setValue={setCacheSize}
          sliderMin={32}
          sliderMax={1024}
          inputMin={32}
          inputMax={999999}
          step={4}
          onBlurCallback={value => setCacheSize(Math.round(value / 4) * 4)}
        />

        <SliderInput
          isProMode={isProMode}
          title={t('SettingsDialog.ReaderReadAHead')}
          value={cachePercentage}
          setValue={setCachePercentage}
          sliderMin={40}
          sliderMax={95}
          inputMin={0}
          inputMax={100}
        />

        <SliderInput
          isProMode={isProMode}
          title={`${t('SettingsDialog.PreloadCache')} - ${preloadCachePercentage}% (${preloadCacheSize} ${t('MB')})`}
          value={preloadCachePercentage}
          setValue={setPreloadCachePercentage}
          sliderMin={0}
          sliderMax={100}
          inputMin={0}
          inputMax={100}
        />
      </div>

      {UseDisk ? (
        <div>
          <CacheStorageLocationLabel />

          <div style={{ display: 'grid', gridAutoFlow: 'column' }}>
            <StorageButton small onClick={() => updateSettings({ UseDisk: false })}>
              <StorageIconWrapper small>
                <RAMIcon color='#323637' />
              </StorageIconWrapper>

              <div>{t('SettingsDialog.RAM')}</div>
            </StorageButton>

            <StorageButton small selected>
              <StorageIconWrapper small selected>
                <USBIcon color='#dee3e5' />
              </StorageIconWrapper>

              <div>{t('SettingsDialog.Disk')}</div>
            </StorageButton>
          </div>

          <FormControlLabel
            control={
              <Switch checked={RemoveCacheOnDrop} onChange={inputForm} id='RemoveCacheOnDrop' color='secondary' />
            }
            label={t('SettingsDialog.RemoveCacheOnDrop')}
            labelPlacement='start'
          />
          <div>
            <small>{t('SettingsDialog.RemoveCacheOnDropDesc')}</small>
          </div>
          <br />
          <TextField
            onChange={inputForm}
            margin='normal'
            id='TorrentsSavePath'
            label={t('SettingsDialog.TorrentsSavePath')}
            value={TorrentsSavePath}
            type='url'
            variant='outlined'
            fullWidth
          />
        </div>
      ) : (
        <CacheStorageSelector>
          <CacheStorageLocationLabel style={{ placeSelf: 'start', gridArea: 'label' }} />

          <StorageButton selected>
            <StorageIconWrapper selected>
              <RAMIcon color='#dee3e5' />
            </StorageIconWrapper>

            <div>{t('SettingsDialog.RAM')}</div>
          </StorageButton>

          <StorageButton onClick={() => updateSettings({ UseDisk: true })}>
            <StorageIconWrapper>
              <USBIcon color='#323637' />
            </StorageIconWrapper>

            <div>{t('SettingsDialog.Disk')}</div>
          </StorageButton>
        </CacheStorageSelector>
      )}
    </MainSettingsContent>
  )
}
